// Command sshcloud-cli is a small test harness for the remote layer. It runs
// one operation against a real server:
//
//	sshcloud-cli -host example.com -user team -root /srv/cloud ls docs
//	sshcloud-cli ... stat docs/report.docx
//	sshcloud-cli ... mv docs/report.docx archive/report.docx
//	sshcloud-cli ... rename docs/report.docx final.docx
//	sshcloud-cli ... rm docs/old
//	sshcloud-cli ... mkdir docs/new
//	sshcloud-cli ... get docs/report.docx report.docx
//	sshcloud-cli ... put report.docx docs/report.docx [replace|keep-both]
//
// get and put use the same transfer list and collision check as the app; the
// list is kept per host in the user's config directory.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mevdschee/ssh-cloud/internal/remote"
	"github.com/mevdschee/ssh-cloud/internal/transfer"
	"github.com/mevdschee/ssh-cloud/internal/transferlog"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func main() {
	home, _ := os.UserHomeDir()
	cfg := remote.Config{UseAgent: true}
	flag.StringVar(&cfg.Host, "host", "", "server host name")
	flag.IntVar(&cfg.Port, "port", 22, "server port")
	flag.StringVar(&cfg.User, "user", "", "username on the server")
	flag.StringVar(&cfg.Root, "root", "", "cloud folder on the server (default: home directory)")
	flag.StringVar(&cfg.KeyFile, "key", "", "private key file (default: ssh-agent only)")
	flag.StringVar(&cfg.KnownHostsFile, "known-hosts", filepath.Join(home, ".ssh", "known_hosts"), "known_hosts file")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: sshcloud-cli [flags] ls|stat|mv|rename|rm|mkdir|get|put args...")
		flag.PrintDefaults()
	}
	flag.Parse()
	if cfg.Host == "" || cfg.User == "" || flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	cfg.TrustHost = askTrust
	cfg.Passphrase = askPassphrase

	c, err := remote.Dial(context.Background(), cfg)
	if err != nil {
		fail(err)
	}
	defer c.Close()

	logDir, _ := os.UserConfigDir()
	log, err := transferlog.Open(filepath.Join(logDir, "SSH-Cloud", "transfers-cli-"+cfg.Host+".jsonl"))
	if err != nil {
		fail(err)
	}
	defer log.Close()

	if err := run(c, transfer.New(c, log), flag.Arg(0), flag.Args()[1:]); err != nil {
		fail(err)
	}
}

func run(c *remote.Client, e *transfer.Engine, cmd string, args []string) error {
	need := func(n int) error {
		if len(args) != n {
			return fmt.Errorf("%s needs %d argument(s)", cmd, n)
		}
		return nil
	}
	switch cmd {
	case "ls":
		dir := ""
		if len(args) > 0 {
			dir = args[0]
		}
		entries, err := c.List(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			kind := "file"
			if e.IsDir {
				kind = "dir "
			}
			fmt.Printf("%s %12d  %s  %s\n", kind, e.Size, e.ModTime.Format("2006-01-02 15:04"), e.Name)
		}
		return nil
	case "stat":
		if err := need(1); err != nil {
			return err
		}
		e, err := c.Stat(args[0])
		if err != nil {
			return err
		}
		fmt.Printf("path: %s\ndir: %v\nsize: %d\nmodified: %s\nmode: %s\n", e.Path, e.IsDir, e.Size, e.ModTime, e.Mode)
		return nil
	case "mv":
		if err := need(2); err != nil {
			return err
		}
		return c.Move(args[0], args[1])
	case "rename":
		if err := need(2); err != nil {
			return err
		}
		return c.Rename(args[0], args[1])
	case "rm":
		if err := need(1); err != nil {
			return err
		}
		return c.Delete(args[0])
	case "mkdir":
		if err := need(1); err != nil {
			return err
		}
		return c.MkdirAll(args[0])
	case "get":
		if err := need(2); err != nil {
			return err
		}
		res, err := e.Download(context.Background(), args[0], args[1])
		if err == nil {
			fmt.Printf("downloaded %s (%d bytes, sha256 %s)\n", res.Path, res.Size, res.Hash)
		}
		return err
	case "put":
		if len(args) < 2 || len(args) > 3 {
			return fmt.Errorf("put needs: local remote [replace|keep-both]")
		}
		mode := transfer.Check
		if len(args) == 3 {
			switch args[2] {
			case "replace":
				mode = transfer.Replace
			case "keep-both":
				mode = transfer.KeepBoth
			default:
				return fmt.Errorf("unknown mode %q", args[2])
			}
		}
		res, err := e.Upload(context.Background(), args[0], args[1], mode)
		if err == nil {
			fmt.Printf("%s: %s (%d bytes)\n", res.Outcome, res.Path, res.Size)
		}
		var ce *transfer.ConflictError
		if errors.As(err, &ce) {
			e.Skip(args[0], args[1])
			return fmt.Errorf("%w; rerun with replace or keep-both", err)
		}
		return err
	}
	return fmt.Errorf("unknown command %q", cmd)
}

func askTrust(host string, key ssh.PublicKey) bool {
	fmt.Fprintf(os.Stderr, "Unknown host %s\n%s key fingerprint %s\nTrust and remember it? [y/N] ",
		host, key.Type(), ssh.FingerprintSHA256(key))
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "y")
}

func askPassphrase(keyFile string) ([]byte, error) {
	fmt.Fprintf(os.Stderr, "Passphrase for %s: ", keyFile)
	pass, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return pass, err
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

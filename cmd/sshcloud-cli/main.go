// Command sshcloud-cli is a small test harness for the remote layer. It runs
// one operation against a real server:
//
//	sshcloud-cli -host example.com -user team -root /srv/cloud ls docs
//	sshcloud-cli ... stat docs/report.docx
//	sshcloud-cli ... mv docs/report.docx archive/report.docx
//	sshcloud-cli ... rename docs/report.docx final.docx
//	sshcloud-cli ... rm docs/old
//	sshcloud-cli ... mkdir docs/new
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mevdschee/ssh-cloud/internal/remote"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func main() {
	home, _ := os.UserHomeDir()
	cfg := remote.Config{UseAgent: true}
	flag.StringVar(&cfg.Host, "host", "", "server host name")
	flag.IntVar(&cfg.Port, "port", 22, "server port")
	flag.StringVar(&cfg.User, "user", "", "shared account name")
	flag.StringVar(&cfg.Root, "root", "", "cloud folder on the server (default: home directory)")
	flag.StringVar(&cfg.KeyFile, "key", "", "private key file (default: ssh-agent only)")
	flag.StringVar(&cfg.KnownHostsFile, "known-hosts", filepath.Join(home, ".ssh", "known_hosts"), "known_hosts file")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: sshcloud-cli [flags] ls|stat|mv|rename|rm|mkdir args...")
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

	if err := run(c, flag.Arg(0), flag.Args()[1:]); err != nil {
		fail(err)
	}
}

func run(c *remote.Client, cmd string, args []string) error {
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

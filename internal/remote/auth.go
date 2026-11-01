package remote

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Authentication methods of a connection profile.
const (
	// AuthAuto tries, in order: the OpenSSH agent, Pageant, the default key
	// files in ~/.ssh, and finally a password prompt.
	AuthAuto = "auto"
	// AuthKeyFile uses one private key file (passphrase prompted if needed).
	AuthKeyFile = "keyfile"
	// AuthPageant uses the keys loaded in PuTTY's Pageant (Windows).
	AuthPageant = "pageant"
	// AuthPassword asks for a password (password and keyboard-interactive).
	AuthPassword = "password"
)

// ErrNoKeys is returned when the chosen method has no keys to offer.
var ErrNoKeys = errors.New("no SSH keys available")

// defaultKeyFiles are tried by AuthAuto, like OpenSSH does.
var defaultKeyFiles = []string{"id_ed25519", "id_ecdsa", "id_rsa"}

// authMethods builds the auth methods for a config. The returned closer
// closes agent connections after the handshake.
func authMethods(cfg Config) ([]ssh.AuthMethod, io.Closer, error) {
	method := cfg.Auth
	if method == "" {
		method = AuthAuto
		if cfg.KeyFile != "" {
			method = AuthKeyFile
		}
	}
	var closers multiCloser

	switch method {
	case AuthKeyFile:
		if cfg.KeyFile == "" {
			return nil, nil, errors.New("no key file chosen")
		}
		s, err := loadKeyFile(cfg, cfg.KeyFile)
		if err != nil {
			return nil, nil, err
		}
		slog.Debug("auth: key file", "file", cfg.KeyFile, "fingerprint", ssh.FingerprintSHA256(s.PublicKey()))
		return []ssh.AuthMethod{ssh.PublicKeys(s)}, nil, nil

	case AuthPageant:
		conn, err := dialPageant()
		if err != nil {
			return nil, nil, fmt.Errorf("Pageant: %w", err)
		}
		signers, err := agent.NewClient(conn).Signers()
		if err != nil || len(signers) == 0 {
			conn.Close()
			if err == nil {
				err = fmt.Errorf("%w in Pageant", ErrNoKeys)
			}
			return nil, nil, err
		}
		slog.Debug("auth: pageant", "keys", len(signers))
		return []ssh.AuthMethod{ssh.PublicKeys(signers...)}, conn, nil

	case AuthPassword:
		if cfg.Password == nil {
			return nil, nil, errors.New("no password prompt available")
		}
		slog.Debug("auth: password")
		return passwordMethods(cfg), nil, nil

	case AuthAuto:
		var signers []ssh.Signer
		if conn, err := dialAgent(); err == nil {
			if s, err := agent.NewClient(conn).Signers(); err == nil && len(s) > 0 {
				signers = append(signers, s...)
				closers = append(closers, conn)
				slog.Debug("auth: ssh-agent", "keys", len(s))
			} else {
				conn.Close()
			}
		}
		if conn, err := dialPageant(); err == nil {
			if s, err := agent.NewClient(conn).Signers(); err == nil && len(s) > 0 {
				signers = append(signers, s...)
				closers = append(closers, conn)
				slog.Debug("auth: pageant", "keys", len(s))
			} else {
				conn.Close()
			}
		}
		signers = append(signers, defaultKeySigners(cfg)...)
		var methods []ssh.AuthMethod
		if len(signers) > 0 {
			methods = append(methods, ssh.PublicKeys(signers...))
		}
		if cfg.Password != nil {
			methods = append(methods, passwordMethods(cfg)...)
		}
		if len(methods) == 0 {
			return nil, nil, fmt.Errorf("%w: no agent, no Pageant, no default key files and no password prompt", ErrNoKeys)
		}
		return methods, closers, nil
	}
	return nil, nil, fmt.Errorf("unknown authentication method %q", method)
}

// passwordMethods answers password and keyboard-interactive authentication
// with prompts to the user. The password is never stored.
func passwordMethods(cfg Config) []ssh.AuthMethod {
	var once sync.Once
	var pw string
	var pwErr error
	ask := func() (string, error) {
		once.Do(func() { pw, pwErr = cfg.Password(fmt.Sprintf("Password for %s@%s", cfg.User, cfg.Host)) })
		return pw, pwErr
	}
	return []ssh.AuthMethod{
		ssh.PasswordCallback(ask),
		ssh.KeyboardInteractive(func(name, instruction string, questions []string, echos []bool) ([]string, error) {
			answers := make([]string, len(questions))
			for i, q := range questions {
				if !echos[i] && len(questions) == 1 {
					a, err := ask() // the usual single "Password:" question
					if err != nil {
						return nil, err
					}
					answers[i] = a
					continue
				}
				a, err := cfg.Password(q)
				if err != nil {
					return nil, err
				}
				answers[i] = a
			}
			return answers, nil
		}),
	}
}

// defaultKeySigners returns signers for the default key files that exist.
// Unencrypted keys are loaded directly; an encrypted key is offered by its
// public key (from the .pub file) and only decrypted, with a passphrase
// prompt, when the server accepts it.
func defaultKeySigners(cfg Config) []ssh.Signer {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var signers []ssh.Signer
	for _, name := range defaultKeyFiles {
		file := filepath.Join(home, ".ssh", name)
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		s, err := ssh.ParsePrivateKey(data)
		if err == nil {
			signers = append(signers, s)
			slog.Debug("auth: default key", "file", file)
			continue
		}
		var missing *ssh.PassphraseMissingError
		if !errors.As(err, &missing) || cfg.Passphrase == nil {
			continue
		}
		pub := missing.PublicKey
		if pub == nil {
			pubData, err := os.ReadFile(file + ".pub")
			if err != nil {
				continue
			}
			if pub, _, _, _, err = ssh.ParseAuthorizedKey(pubData); err != nil {
				continue
			}
		}
		signers = append(signers, &lazySigner{pub: pub, load: func() (ssh.Signer, error) { return loadKeyFile(cfg, file) }})
		slog.Debug("auth: encrypted default key (decrypted on demand)", "file", file)
	}
	return signers
}

// lazySigner decrypts its key on the first signature.
type lazySigner struct {
	pub    ssh.PublicKey
	load   func() (ssh.Signer, error)
	once   sync.Once
	signer ssh.Signer
	err    error
}

func (l *lazySigner) PublicKey() ssh.PublicKey { return l.pub }

func (l *lazySigner) Sign(rand io.Reader, data []byte) (*ssh.Signature, error) {
	l.once.Do(func() { l.signer, l.err = l.load() })
	if l.err != nil {
		return nil, l.err
	}
	return l.signer.Sign(rand, data)
}

// SignWithAlgorithm makes RSA keys use rsa-sha2 signatures.
func (l *lazySigner) SignWithAlgorithm(rand io.Reader, data []byte, algorithm string) (*ssh.Signature, error) {
	l.once.Do(func() { l.signer, l.err = l.load() })
	if l.err != nil {
		return nil, l.err
	}
	if as, ok := l.signer.(ssh.AlgorithmSigner); ok {
		return as.SignWithAlgorithm(rand, data, algorithm)
	}
	return l.signer.Sign(rand, data)
}

func loadKeyFile(cfg Config, file string) (ssh.Signer, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read key file: %w", err)
	}
	s, err := ssh.ParsePrivateKey(data)
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		if cfg.Passphrase == nil {
			return nil, fmt.Errorf("key file %s is encrypted and no passphrase prompt is set", file)
		}
		pass, perr := cfg.Passphrase(file)
		if perr != nil {
			return nil, perr
		}
		s, err = ssh.ParsePrivateKeyWithPassphrase(data, pass)
	}
	if err != nil {
		return nil, fmt.Errorf("parse key file: %w", err)
	}
	return s, nil
}

type multiCloser []io.Closer

func (m multiCloser) Close() error {
	for _, c := range m {
		c.Close()
	}
	return nil
}

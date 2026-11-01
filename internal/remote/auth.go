package remote

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// ErrNoKeys is returned when neither the agent nor a key file provided a key.
var ErrNoKeys = errors.New("no SSH keys available (agent empty and no key file)")

// authMethods builds the public key auth method. Only public key auth is
// supported: the key is the user's identity on the shared account.
func authMethods(cfg Config) ([]ssh.AuthMethod, io.Closer, error) {
	var signers []ssh.Signer
	var closer io.Closer

	if cfg.KeyFile != "" {
		s, err := loadKeyFile(cfg)
		if err != nil {
			return nil, nil, err
		}
		signers = append(signers, s)
	}

	if cfg.UseAgent {
		conn, err := dialAgent()
		if err == nil {
			closer = conn
			agentSigners, err := agent.NewClient(conn).Signers()
			if err == nil {
				signers = append(signers, agentSigners...)
			}
		}
		// A missing agent is not fatal when a key file was given.
	}

	if len(signers) == 0 {
		if closer != nil {
			closer.Close()
		}
		return nil, nil, ErrNoKeys
	}
	return []ssh.AuthMethod{ssh.PublicKeys(signers...)}, closer, nil
}

func loadKeyFile(cfg Config) (ssh.Signer, error) {
	data, err := os.ReadFile(cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("read key file: %w", err)
	}
	s, err := ssh.ParsePrivateKey(data)
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		if cfg.Passphrase == nil {
			return nil, fmt.Errorf("key file %s is encrypted and no passphrase prompt is set", cfg.KeyFile)
		}
		pass, perr := cfg.Passphrase(cfg.KeyFile)
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

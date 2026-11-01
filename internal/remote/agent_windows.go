//go:build windows

package remote

import (
	"io"
	"os"
)

// The Windows OpenSSH agent listens on a named pipe. A byte-mode named pipe
// can be opened like a regular file, which is enough for the agent protocol
// (strict request/response).
const windowsAgentPipe = `\\.\pipe\openssh-ssh-agent`

func dialAgent() (io.ReadWriteCloser, error) {
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if f, err := os.OpenFile(sock, os.O_RDWR, 0); err == nil {
			return f, nil
		}
	}
	return os.OpenFile(windowsAgentPipe, os.O_RDWR, 0)
}

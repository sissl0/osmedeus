package cloud

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/j3ssie/osmedeus/v5/internal/core"
	"github.com/j3ssie/osmedeus/v5/internal/runner"
	"github.com/j3ssie/osmedeus/v5/internal/terminal"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// ansiControlRegex strips non-visual ANSI sequences (cursor, screen, OSC, mode)
// while preserving SGR sequences (colors/bold/reset ending in 'm').
// OSC sequences can end with BEL (\x07) or ST (\x1b\\).
var ansiControlRegex = regexp.MustCompile(`\x1b\[[0-9;]*[A-HJ-Zadfhijklnqrstu]|\x1b\][^\x1b\x07]*(?:\x07|\x1b\\)|\x1b\(B|\x1b\[\?[0-9;]*[hl]`)

// ansiAllRegex strips all ANSI escape sequences including colors (used when color is disabled).
var ansiAllRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x1b\x07]*(?:\x07|\x1b\\)|\x1b\(B|\x1b\[[\?]?[0-9;]*[hlm]`)

// SSHConfig holds the parameters needed to connect to a remote host
type SSHConfig struct {
	Host     string
	Port     int
	User     string
	KeyFile  string
	Password string
}

// CloudSSHClient wraps a pooled SSH connection for cloud operations.
// It provides command execution (blocking and streaming) plus SFTP file transfers.
type CloudSSHClient struct {
	client  *ssh.Client
	poolKey runner.SSHPoolKey
	config  *core.RunnerConfig
}

// NewCloudSSHClient creates a new SSH client using the global connection pool.
func NewCloudSSHClient(ctx context.Context, cfg SSHConfig) (*CloudSSHClient, error) {
	if cfg.Port == 0 {
		cfg.Port = 22
	}
	if cfg.User == "" {
		cfg.User = "root"
	}

	runnerCfg := &core.RunnerConfig{
		Host:     cfg.Host,
		Port:     cfg.Port,
		User:     cfg.User,
		KeyFile:  cfg.KeyFile,
		Password: cfg.Password,
	}

	pool := runner.GetSSHPool()
	client, poolKey, err := pool.Get(ctx, runnerCfg)
	if err != nil {
		return nil, fmt.Errorf("SSH connection to %s:%d failed: %w", cfg.Host, cfg.Port, err)
	}

	return &CloudSSHClient{
		client:  client,
		poolKey: poolKey,
		config:  runnerCfg,
	}, nil
}

// Close releases the connection back to the pool.
func (c *CloudSSHClient) Close() {
	runner.GetSSHPool().Release(c.poolKey)
}

// RunCommand executes a command on the remote host and returns the output.
func (c *CloudSSHClient) RunCommand(ctx context.Context, command string) (string, int, error) {
	session, err := c.client.NewSession()
	if err != nil {
		return "", -1, fmt.Errorf("failed to create session: %w", err)
	}
	defer func() { _ = session.Close() }()

	// Request a PTY so sudo works (non-fatal if unavailable)
	_ = session.RequestPty("xterm", 80, 200, ssh.TerminalModes{})

	output, err := session.CombinedOutput(command)
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*ssh.ExitError); ok {
			exitCode = exitErr.ExitStatus()
		} else {
			return string(output), -1, err
		}
	}

	return string(output), exitCode, nil
}

// LineCallback is called for each ANSI-stripped output line during streaming.
type LineCallback func(line string)

// RunCommandStreaming executes a command, streaming stdout/stderr line-by-line with a prefix.
// Blocks until the command completes.
func (c *CloudSSHClient) RunCommandStreaming(ctx context.Context, command string, prefix string) error {
	return c.RunCommandStreamingWithCallback(ctx, command, prefix, nil)
}

// RunCommandStreamingWithCallback is like RunCommandStreaming but calls onLine for each
// ANSI-stripped output line, allowing the caller to observe progress without parsing stdout.
func (c *CloudSSHClient) RunCommandStreamingWithCallback(ctx context.Context, command string, prefix string, onLine LineCallback) error {
	session, err := c.client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	defer func() { _ = session.Close() }()

	// Request a PTY so sudo works and output is interleaved (non-fatal if unavailable)
	_ = session.RequestPty("xterm", 80, 200, ssh.TerminalModes{})

	stdout, err := session.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe: %w", err)
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	if err := session.Start(command); err != nil {
		return fmt.Errorf("failed to start command: %w", err)
	}

	prefixStr := terminal.Gray(fmt.Sprintf("[%s] ", prefix))

	// oscFragmentRegex catches leftover OSC fragments when the ESC opener was on a previous line
	// e.g. "11;rgb:1818/1919/2020" from a split OSC 11 background-color response
	oscFragmentRegex := regexp.MustCompile(`^[0-9]+;[^\x1b]*(?:\x07|\x1b\\)?`)

	// Stream both pipes with prefix in parallel
	done := make(chan struct{}, 2)
	streamLines := func(r io.Reader) {
		defer func() { done <- struct{}{} }()
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		pendingOSC := false
		for scanner.Scan() {
			raw := scanner.Text()

			// If previous line had an unterminated OSC, this line is the continuation
			if pendingOSC {
				pendingOSC = false
				raw = oscFragmentRegex.ReplaceAllString(raw, "")
			}

			// Detect unterminated OSC at end of line (ESC ] without matching terminator)
			if idx := strings.LastIndex(raw, "\x1b]"); idx >= 0 {
				tail := raw[idx:]
				if !strings.Contains(tail, "\x07") && !strings.Contains(tail, "\x1b\\") {
					raw = raw[:idx]
					pendingOSC = true
				}
			}

			var line string
			if terminal.IsColorEnabled() {
				// Preserve SGR (color) codes; strip only control sequences
				line = ansiControlRegex.ReplaceAllString(raw, "")
			} else {
				line = ansiAllRegex.ReplaceAllString(raw, "")
			}
			if line == "" {
				continue
			}
			_, _ = fmt.Fprintf(os.Stdout, "%s%s\n", prefixStr, line)
			if onLine != nil {
				onLine(line)
			}
		}
	}

	go streamLines(stdout)
	go streamLines(stderr)

	<-done
	<-done

	return session.Wait()
}

// UploadFile copies a local file to the remote host via SFTP.
func (c *CloudSSHClient) UploadFile(localPath, remotePath string) error {
	sftpClient, err := sftp.NewClient(c.client)
	if err != nil {
		return fmt.Errorf("failed to create SFTP client: %w", err)
	}
	defer func() { _ = sftpClient.Close() }()

	// Ensure remote directory exists
	remoteDir := filepath.Dir(remotePath)
	_ = sftpClient.MkdirAll(remoteDir)

	localFile, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("failed to open local file: %w", err)
	}
	defer func() { _ = localFile.Close() }()

	remoteFile, err := sftpClient.Create(remotePath)
	if err != nil {
		return fmt.Errorf("failed to create remote file: %w", err)
	}
	defer func() { _ = remoteFile.Close() }()

	if _, err := io.Copy(remoteFile, localFile); err != nil {
		return fmt.Errorf("failed to upload file: %w", err)
	}

	return nil
}

// DownloadFile copies a remote file to the local filesystem via SFTP.
func (c *CloudSSHClient) DownloadFile(remotePath, localPath string) error {
	sftpClient, err := sftp.NewClient(c.client)
	if err != nil {
		return fmt.Errorf("failed to create SFTP client: %w", err)
	}
	defer func() { _ = sftpClient.Close() }()

	remoteFile, err := sftpClient.Open(remotePath)
	if err != nil {
		return fmt.Errorf("failed to open remote file: %w", err)
	}
	defer func() { _ = remoteFile.Close() }()

	// Ensure local directory exists
	localDir := filepath.Dir(localPath)
	if err := os.MkdirAll(localDir, 0755); err != nil {
		return fmt.Errorf("failed to create local directory: %w", err)
	}

	localFile, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("failed to create local file: %w", err)
	}
	defer func() { _ = localFile.Close() }()

	if _, err := io.Copy(localFile, remoteFile); err != nil {
		return fmt.Errorf("failed to download file: %w", err)
	}

	return nil
}

// DownloadDir recursively downloads a remote directory to local via SFTP.
func (c *CloudSSHClient) DownloadDir(remotePath, localPath string) error {
	sftpClient, err := sftp.NewClient(c.client)
	if err != nil {
		return fmt.Errorf("failed to create SFTP client: %w", err)
	}
	defer func() { _ = sftpClient.Close() }()

	return downloadDirRecursive(sftpClient, remotePath, localPath)
}

func downloadDirRecursive(client *sftp.Client, remotePath, localPath string) error {
	entries, err := client.ReadDir(remotePath)
	if err != nil {
		return fmt.Errorf("failed to list remote dir %s: %w", remotePath, err)
	}

	if err := os.MkdirAll(localPath, 0755); err != nil {
		return fmt.Errorf("failed to create local dir: %w", err)
	}

	for _, entry := range entries {
		remoteEntry := remotePath + "/" + entry.Name()
		localEntry := filepath.Join(localPath, entry.Name())

		if entry.IsDir() {
			if err := downloadDirRecursive(client, remoteEntry, localEntry); err != nil {
				return err
			}
		} else {
			remoteFile, err := client.Open(remoteEntry)
			if err != nil {
				return fmt.Errorf("failed to open %s: %w", remoteEntry, err)
			}

			localFile, err := os.Create(localEntry)
			if err != nil {
				_ = remoteFile.Close()
				return fmt.Errorf("failed to create %s: %w", localEntry, err)
			}

			_, copyErr := io.Copy(localFile, remoteFile)
			_ = remoteFile.Close()
			_ = localFile.Close()
			if copyErr != nil {
				return fmt.Errorf("failed to copy %s: %w", remoteEntry, copyErr)
			}
		}
	}
	return nil
}

// ExpandPath expands ~ to the user's home directory
func ExpandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

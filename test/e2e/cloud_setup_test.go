package e2e

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	cloudTestSSHHost = "localhost"
	cloudTestSSHPort = "2222"
	cloudTestSSHUser = "testuser"
	cloudTestSSHPass = "testpass"
)

// startCloudTestSSHServer starts the SSH test container and returns a cleanup func
func startCloudTestSSHServer(t *testing.T) func() {
	t.Helper()
	composePath := filepath.Join(getProjectRoot(t), "build", "docker", "docker-compose.test.yaml")

	cmd := exec.Command("docker-compose", "-f", composePath, "up", "-d", "ssh-server")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to start SSH container: %v\nOutput: %s", err, output)
	}

	// Wait for SSH to be ready
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		conn, connErr := net.DialTimeout("tcp", cloudTestSSHHost+":"+cloudTestSSHPort, time.Second)
		if connErr == nil {
			_ = conn.Close()
			// Extra wait for sshd to fully initialize
			time.Sleep(2 * time.Second)
			return func() {
				cmd := exec.Command("docker-compose", "-f", composePath, "down", "-v")
				_ = cmd.Run()
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatal("SSH server did not become ready in time")
	return nil
}

// requireSshpass checks that sshpass is installed
func requireSshpass(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sshpass"); err != nil {
		t.Skip("sshpass not installed — skipping password auth test")
	}
}

// TestCloudSetup_SSHPassword tests running commands on the SSH container via password auth
func TestCloudSetup_SSHPassword(t *testing.T) {
	requireSshpass(t)
	cleanup := startCloudTestSSHServer(t)
	defer cleanup()

	// Run a simple command via sshpass + ssh
	sshCmd := fmt.Sprintf(
		"sshpass -p '%s' ssh -tt -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -p %s %s@%s 'echo CLOUD_SETUP_OK'",
		cloudTestSSHPass, cloudTestSSHPort, cloudTestSSHUser, cloudTestSSHHost,
	)
	out, err := exec.Command("bash", "-c", sshCmd).CombinedOutput()
	require.NoError(t, err, "SSH password auth failed: %s", string(out))
	assert.Contains(t, string(out), "CLOUD_SETUP_OK")

	// Run multiple commands (simulating setup.commands)
	commands := []string{
		"echo STEP_1_DONE",
		"echo STEP_2_DONE",
		"whoami",
	}
	for _, cmd := range commands {
		fullCmd := fmt.Sprintf(
			"sshpass -p '%s' ssh -tt -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -p %s %s@%s '%s'",
			cloudTestSSHPass, cloudTestSSHPort, cloudTestSSHUser, cloudTestSSHHost, cmd,
		)
		out, err := exec.Command("bash", "-c", fullCmd).CombinedOutput()
		require.NoError(t, err, "Command '%s' failed: %s", cmd, string(out))
	}
}

// TestCloudSetup_SSHKey tests running commands on the SSH container via key auth
func TestCloudSetup_SSHKey(t *testing.T) {
	cleanup := startCloudTestSSHServer(t)
	defer cleanup()

	// Generate a temp SSH key pair
	tmpDir := t.TempDir()
	keyPath := filepath.Join(tmpDir, "test_key")
	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-f", keyPath, "-N", "", "-q")
	require.NoError(t, cmd.Run(), "Failed to generate SSH key")

	pubKeyBytes, err := os.ReadFile(keyPath + ".pub")
	require.NoError(t, err)
	pubKey := strings.TrimSpace(string(pubKeyBytes))

	// Add the public key to the container's authorized_keys via password auth
	requireSshpass(t)
	addKeyCmd := fmt.Sprintf(
		"sshpass -p '%s' ssh -tt -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -p %s %s@%s 'mkdir -p ~/.ssh && echo \"%s\" >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys'",
		cloudTestSSHPass, cloudTestSSHPort, cloudTestSSHUser, cloudTestSSHHost, pubKey,
	)
	out, err := exec.Command("bash", "-c", addKeyCmd).CombinedOutput()
	require.NoError(t, err, "Failed to add key to container: %s", string(out))

	// Now test key-based auth
	sshCmd := fmt.Sprintf(
		"ssh -tt -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -i %s -p %s %s@%s 'echo KEY_AUTH_OK'",
		keyPath, cloudTestSSHPort, cloudTestSSHUser, cloudTestSSHHost,
	)
	out, err = exec.Command("bash", "-c", sshCmd).CombinedOutput()
	require.NoError(t, err, "SSH key auth failed: %s", string(out))
	assert.Contains(t, string(out), "KEY_AUTH_OK")
}

// TestCloudSetup_PostCommandVars tests that template variables in post_commands are expanded
func TestCloudSetup_PostCommandVars(t *testing.T) {
	requireSshpass(t)
	cleanup := startCloudTestSSHServer(t)
	defer cleanup()

	vars := map[string]string{
		"public_ip":   "10.0.0.1",
		"worker_name": "osmw-test-0",
		"infra_id":    "cloud-test-123",
		"provider":    "remote-adhoc",
		"ssh_user":    cloudTestSSHUser,
		"index":       "0",
	}

	// Test variable expansion
	template := "echo IP={{public_ip}} NAME={{worker_name}} INFRA={{infra_id}} PROVIDER={{provider}}"
	expanded := template
	for k, v := range vars {
		expanded = strings.ReplaceAll(expanded, "{{"+k+"}}", v)
	}

	assert.Contains(t, expanded, "IP=10.0.0.1")
	assert.Contains(t, expanded, "NAME=osmw-test-0")
	assert.Contains(t, expanded, "INFRA=cloud-test-123")
	assert.Contains(t, expanded, "PROVIDER=remote-adhoc")

	// Run the expanded command on the SSH container
	sshCmd := fmt.Sprintf(
		"sshpass -p '%s' ssh -tt -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -p %s %s@%s '%s'",
		cloudTestSSHPass, cloudTestSSHPort, cloudTestSSHUser, cloudTestSSHHost, expanded,
	)
	out, err := exec.Command("bash", "-c", sshCmd).CombinedOutput()
	require.NoError(t, err, "Post-command failed: %s", string(out))
	assert.Contains(t, string(out), "IP=10.0.0.1")
	assert.Contains(t, string(out), "NAME=osmw-test-0")
}

// TestCloudSetup_Ansible tests running an ansible playbook against the SSH container
func TestCloudSetup_Ansible(t *testing.T) {
	// Check ansible is installed
	if _, err := exec.LookPath("ansible-playbook"); err != nil {
		t.Skip("ansible-playbook not installed — skipping ansible test")
	}

	cleanup := startCloudTestSSHServer(t)
	defer cleanup()

	tmpDir := t.TempDir()

	// Create a simple test playbook (use raw module — no Python needed in container)
	playbook := `---
- name: Test Cloud Setup
  hosts: osmedeus_workers
  gather_facts: no
  tasks:
    - name: Echo test
      raw: echo ANSIBLE_OK
      register: result
    - name: Show result
      debug:
        msg: "{{ result.stdout }}"
`
	playbookPath := filepath.Join(tmpDir, "test-playbook.yaml")
	require.NoError(t, os.WriteFile(playbookPath, []byte(playbook), 0644))

	// Create inventory with password auth
	inventory := fmt.Sprintf(`[osmedeus_workers]
%s ansible_user=%s ansible_ssh_pass=%s ansible_port=%s

[osmedeus_workers:vars]
ansible_ssh_common_args='-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR'
`, cloudTestSSHHost, cloudTestSSHUser, cloudTestSSHPass, cloudTestSSHPort)

	inventoryPath := filepath.Join(tmpDir, "inventory.ini")
	require.NoError(t, os.WriteFile(inventoryPath, []byte(inventory), 0644))

	// Run ansible-playbook
	cmd := exec.Command("ansible-playbook", "-i", inventoryPath, playbookPath)
	out, err := cmd.CombinedOutput()
	t.Logf("Ansible output:\n%s", string(out))
	require.NoError(t, err, "Ansible playbook failed: %s", string(out))
	assert.Contains(t, string(out), "ANSIBLE_OK")
}

// sshReadFile reads a file from the test SSH container via sshpass
func sshReadFile(t *testing.T, path string) string {
	t.Helper()
	sshCmd := fmt.Sprintf(
		"sshpass -p '%s' ssh -tt -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -p %s %s@%s 'cat %s'",
		cloudTestSSHPass, cloudTestSSHPort, cloudTestSSHUser, cloudTestSSHHost, path,
	)
	out, err := exec.Command("bash", "-c", sshCmd).CombinedOutput()
	require.NoError(t, err, "Failed to read %s via SSH: %s", path, string(out))
	return strings.TrimSpace(string(out))
}

// TestCloudSetup_FullCLIFlow tests the full cloud setup CLI flow:
// configure cloud settings, run `osmedeus cloud setup`, verify results via SSH
func TestCloudSetup_FullCLIFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cloud setup E2E test in short mode")
	}
	requireSshpass(t)
	cleanup := startCloudTestSSHServer(t)
	defer cleanup()

	log := NewTestLogger(t)
	baseDir := t.TempDir()

	// Configure cloud settings
	log.Step("Configuring cloud settings")
	configSets := [][]string{
		{"cloud", "config", "set", "ssh.password", cloudTestSSHPass},
		{"cloud", "config", "set", "ssh.user", cloudTestSSHUser},
		{"cloud", "config", "set", "ssh.port", cloudTestSSHPort},
		{"cloud", "config", "set", "setup.commands.add", "echo SETUP_MARKER > /tmp/setup-ok"},
		{"cloud", "config", "set", "setup.commands.add", "echo STEP_TWO >> /tmp/setup-ok"},
		{"cloud", "config", "set", "setup.post_commands.add", "echo IP={{public_ip}} > /tmp/post-ok"},
	}
	for _, args := range configSets {
		stdout, _, err := runCLIInBase(t, log, baseDir, args...)
		require.NoError(t, err, "config set failed: %s", stdout)
	}

	// Run cloud setup against the SSH container
	log.Step("Running cloud setup")
	stdout, _, err := runCLIInBase(t, log, baseDir, "cloud", "setup", cloudTestSSHHost)
	require.NoError(t, err, "cloud setup failed: %s", stdout)
	assert.Contains(t, stdout, "Setup complete")

	// Verify setup commands ran
	log.Step("Verifying setup results")
	marker := sshReadFile(t, "/tmp/setup-ok")
	assert.Contains(t, marker, "SETUP_MARKER")
	assert.Contains(t, marker, "STEP_TWO")

	// Verify post-commands ran with variable expansion
	postMarker := sshReadFile(t, "/tmp/post-ok")
	assert.Contains(t, postMarker, "IP="+cloudTestSSHHost)

	log.Success("Full CLI flow cloud setup works correctly")
}

// TestCloudSetup_ClearCommandsThenSetup tests that clearing setup commands works
// and that cloud setup handles empty command lists gracefully
func TestCloudSetup_ClearCommandsThenSetup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cloud setup E2E test in short mode")
	}
	requireSshpass(t)
	cleanup := startCloudTestSSHServer(t)
	defer cleanup()

	log := NewTestLogger(t)
	baseDir := t.TempDir()

	// Add then clear setup commands
	log.Step("Adding and clearing setup commands")
	_, _, err := runCLIInBase(t, log, baseDir, "cloud", "config", "set", "ssh.password", cloudTestSSHPass)
	require.NoError(t, err)
	_, _, err = runCLIInBase(t, log, baseDir, "cloud", "config", "set", "ssh.user", cloudTestSSHUser)
	require.NoError(t, err)
	_, _, err = runCLIInBase(t, log, baseDir, "cloud", "config", "set", "ssh.port", cloudTestSSHPort)
	require.NoError(t, err)
	_, _, err = runCLIInBase(t, log, baseDir, "cloud", "config", "set", "setup.commands.add", "echo SHOULD_NOT_RUN")
	require.NoError(t, err)
	_, _, err = runCLIInBase(t, log, baseDir, "cloud", "config", "set", "setup.commands.clear")
	require.NoError(t, err)

	// Run cloud setup — should report no commands
	log.Step("Running cloud setup with cleared commands")
	stdout, _, err := runCLIInBase(t, log, baseDir, "cloud", "setup", cloudTestSSHHost)
	require.NoError(t, err, "cloud setup failed: %s", stdout)
	assert.Contains(t, stdout, "No setup commands configured")

	log.Success("Clear commands + setup works correctly")
}

// TestCloudSetup_AnsibleCLIFlow tests running cloud setup with --ansible flag via the CLI
func TestCloudSetup_AnsibleCLIFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cloud setup E2E test in short mode")
	}
	if _, err := exec.LookPath("ansible-playbook"); err != nil {
		t.Skip("ansible-playbook not installed — skipping ansible CLI test")
	}
	requireSshpass(t)
	cleanup := startCloudTestSSHServer(t)
	defer cleanup()

	log := NewTestLogger(t)
	baseDir := t.TempDir()

	// Create a test playbook that writes a marker file
	playbookDir := filepath.Join(baseDir, "cloud-infra")
	require.NoError(t, os.MkdirAll(playbookDir, 0755))
	playbook := `---
- name: Test Cloud Setup via CLI
  hosts: osmedeus_workers
  gather_facts: no
  tasks:
    - name: Write ansible marker
      raw: echo ANSIBLE_CLI_OK > /tmp/ansible-marker
`
	playbookPath := filepath.Join(playbookDir, "setup-playbook.yaml")
	require.NoError(t, os.WriteFile(playbookPath, []byte(playbook), 0644))

	// Configure cloud settings with ansible
	log.Step("Configuring cloud settings for ansible")
	configSets := [][]string{
		{"cloud", "config", "set", "ssh.password", cloudTestSSHPass},
		{"cloud", "config", "set", "ssh.user", cloudTestSSHUser},
		{"cloud", "config", "set", "ssh.port", cloudTestSSHPort},
		{"cloud", "config", "set", "setup.ansible.enabled", "true"},
		{"cloud", "config", "set", "setup.ansible.playbook_path", playbookPath},
	}
	for _, args := range configSets {
		_, _, err := runCLIInBase(t, log, baseDir, args...)
		require.NoError(t, err)
	}

	// Run cloud setup with ansible
	log.Step("Running cloud setup with ansible")
	stdout, _, err := runCLIInBase(t, log, baseDir, "cloud", "setup", cloudTestSSHHost, "--ansible")
	require.NoError(t, err, "cloud setup --ansible failed: %s", stdout)

	// Verify ansible playbook ran
	log.Step("Verifying ansible results")
	marker := sshReadFile(t, "/tmp/ansible-marker")
	assert.Contains(t, marker, "ANSIBLE_CLI_OK")

	log.Success("Ansible CLI flow cloud setup works correctly")
}

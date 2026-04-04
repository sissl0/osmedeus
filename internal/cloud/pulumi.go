package cloud

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optdestroy"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// PulumiManager manages Pulumi stacks for infrastructure provisioning
type PulumiManager struct {
	projectName string
	stackName   string
	statePath   string
	workspace   auto.Workspace
	stack       auto.Stack
}

// NewPulumiManager creates a new Pulumi manager with local backend
func NewPulumiManager(projectName, stackName, statePath string) (*PulumiManager, error) {
	// Ensure Pulumi is installed
	if err := ensurePulumiInstalled(); err != nil {
		return nil, err
	}

	// Ensure state directory exists
	if err := os.MkdirAll(statePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create state directory: %w", err)
	}

	// Set passphrase for local secrets encryption if not already set.
	// Pulumi requires this for its local backend to encrypt config secrets.
	if os.Getenv("PULUMI_CONFIG_PASSPHRASE") == "" && os.Getenv("PULUMI_CONFIG_PASSPHRASE_FILE") == "" {
		_ = os.Setenv("PULUMI_CONFIG_PASSPHRASE", "")
	}

	ctx := context.Background()

	// Use file:// backend pointing at the state directory
	backendURL := fmt.Sprintf("file://%s", statePath)

	// Initialize stack with inline program and explicit local backend
	stack, err := auto.UpsertStackInlineSource(ctx, stackName, projectName, func(ctx *pulumi.Context) error {
		return nil
	}, auto.EnvVars(map[string]string{
		"PULUMI_BACKEND_URL":        backendURL,
		"PULUMI_CONFIG_PASSPHRASE":  os.Getenv("PULUMI_CONFIG_PASSPHRASE"),
	}))
	if err != nil {
		return nil, fmt.Errorf("failed to create stack: %w", err)
	}

	return &PulumiManager{
		projectName: projectName,
		stackName:   stackName,
		statePath:   statePath,
		workspace:   stack.Workspace(),
		stack:       stack,
	}, nil
}

// Up provisions infrastructure using the provided Pulumi program
func (pm *PulumiManager) Up(ctx context.Context, program pulumi.RunFunc) error {
	// Set the program for the stack
	pm.stack.Workspace().SetProgram(program)

	// Set stack configuration if needed
	// This can be extended to set provider-specific config

	// Run pulumi up with colorized progress streaming
	pw := NewPulumiWriter()
	_, err := pm.stack.Up(ctx, optup.ProgressStreams(pw))
	pw.Flush()
	if err != nil {
		return fmt.Errorf("failed to provision infrastructure: %w", err)
	}

	return nil
}

// Destroy tears down the infrastructure and removes the stack and its state
func (pm *PulumiManager) Destroy(ctx context.Context) error {
	pw := NewPulumiWriter()
	_, err := pm.stack.Destroy(ctx, optdestroy.ProgressStreams(pw))
	pw.Flush()
	if err != nil {
		return fmt.Errorf("failed to destroy infrastructure: %w", err)
	}

	// Remove stack history and configuration after successful destroy
	if err := pm.stack.Workspace().RemoveStack(ctx, pm.stackName); err != nil {
		return fmt.Errorf("failed to remove stack: %w", err)
	}

	// Clean up leftover Pulumi state directory for this stack
	pm.cleanupStackState()

	return nil
}

// cleanupStackState removes leftover Pulumi local backend files for the stack
func (pm *PulumiManager) cleanupStackState() {
	statePath := pm.statePath

	// Remove stack-specific state files from the local backend
	stackDir := filepath.Join(statePath, ".pulumi", "stacks", pm.projectName)
	stackFile := filepath.Join(stackDir, pm.stackName+".json")
	_ = os.Remove(stackFile)
	stackFileBak := filepath.Join(stackDir, pm.stackName+".json.bak")
	_ = os.Remove(stackFileBak)

	// Remove the project directory if empty
	entries, err := os.ReadDir(stackDir)
	if err == nil && len(entries) == 0 {
		_ = os.Remove(stackDir)
	}
}

// GetOutputs retrieves the stack outputs (IPs, IDs, etc.)
func (pm *PulumiManager) GetOutputs(ctx context.Context) (map[string]auto.OutputValue, error) {
	outputs, err := pm.stack.Outputs(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get stack outputs: %w", err)
	}
	return outputs, nil
}

// SetConfig sets a Pulumi stack configuration value
func (pm *PulumiManager) SetConfig(ctx context.Context, key, value string, secret bool) error {
	return pm.stack.SetConfig(ctx, key, auto.ConfigValue{
		Value:  value,
		Secret: secret,
	})
}

// GetStackName returns the stack name
func (pm *PulumiManager) GetStackName() string {
	return pm.stackName
}

// ensurePulumiInstalled checks if Pulumi CLI is installed
func ensurePulumiInstalled() error {
	// Check if pulumi is in PATH
	if _, err := exec.LookPath("pulumi"); err == nil {
		return nil
	}

	// Check in common installation paths
	commonPaths := []string{
		filepath.Join(os.Getenv("HOME"), ".pulumi", "bin", "pulumi"),
		"/usr/local/bin/pulumi",
		"/opt/homebrew/bin/pulumi",
	}

	for _, path := range commonPaths {
		if _, err := os.Stat(path); err == nil {
			// Add to PATH for current session
			currentPath := os.Getenv("PATH")
			if err := os.Setenv("PATH", filepath.Dir(path)+":"+currentPath); err != nil {
				return fmt.Errorf("failed to set PATH: %w", err)
			}
			return nil
		}
	}

	return fmt.Errorf("pulumi CLI not found - install via: curl -fsSL https://get.pulumi.com | sh")
}

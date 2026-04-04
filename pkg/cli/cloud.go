package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	execPkg "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-yaml/parser"
	"github.com/google/uuid"
	"github.com/j3ssie/osmedeus/v5/internal/cloud"
	"github.com/j3ssie/osmedeus/v5/internal/config"
	"github.com/j3ssie/osmedeus/v5/internal/database"
	"github.com/j3ssie/osmedeus/v5/internal/snapshot"
	"github.com/j3ssie/osmedeus/v5/internal/terminal"
	"github.com/j3ssie/osmedeus/v5/public"
	"github.com/spf13/cobra"
)

var (
	// Cloud command flags
	cloudProvider  string
	cloudMode      string
	cloudInstances int
	cloudForce     bool

	// Cloud run flags
	cloudFlowName      string
	cloudModuleName    string
	cloudTarget        string
	cloudTargetFile    string
	cloudTimeout       string
	cloudAutoDestroy   bool
	cloudReuseInfra    bool
	cloudReuseWith     string
	cloudVerboseSetup  bool
	cloudUseAnsible    bool
	cloudSkipSetup     bool
	cloudSyncBack      bool

	// Cloud run chunk flags
	cloudChunkSize  int
	cloudChunkCount int

	// Custom command mode flags
	cloudCustomCmds     []string // --custom-cmd (repeatable)
	cloudCustomPostCmds []string // --custom-post-cmd (repeatable)
	cloudSyncPaths      []string // --sync-path (repeatable)
	cloudSyncDest       string   // --sync-dest (default "./osm-sync-back")

	// Cloud config set flags
	cloudConfigSetFromFile string
)


// cloudCmd represents the cloud command
var cloudCmd = &cobra.Command{
	Use:   "cloud",
	Short: "Cloud infrastructure management commands",
	Long: terminal.BoldCyan("◆ Description") + `
  Provision and manage cloud infrastructure for distributed scanning.
  Supports AWS, GCP, DigitalOcean, Linode, Azure, and Hetzner.

` + terminal.BoldCyan("▷ Quick Start") + `
  # 1. Configure provider credentials
  ` + terminal.Green("osmedeus cloud config set providers.aws.access_key_id <key>") + `
  ` + terminal.Green("osmedeus cloud config set providers.aws.secret_access_key <secret>") + `
  ` + terminal.Green("osmedeus cloud config set providers.aws.region ap-southeast-1") + `
  ` + terminal.Green("osmedeus cloud config set defaults.provider aws") + `

  # 2. Configure SSH keys
  ` + terminal.Green("osmedeus cloud config set ssh.private_key_path ~/.ssh/id_rsa") + `
  ` + terminal.Green("osmedeus cloud config set ssh.public_key_path ~/.ssh/id_rsa.pub") + `

  # 3. Add setup commands (runs on each worker before scanning)
  ` + terminal.Green(`osmedeus cloud config set setup.commands.add "curl -fsSL https://www.osmedeus.org/install.sh | bash"`) + `
  ` + terminal.Green(`osmedeus cloud config set setup.commands.add "osmedeus install base --preset"`) + `

  # 4. Run a scan on cloud infrastructure
  ` + terminal.Green("osmedeus cloud run -f fast -t example.com --instances 1") + `

` + terminal.BoldCyan("▷ Common Commands") + `
  ` + terminal.Green("osmedeus cloud config list") + `              Show cloud configuration
  ` + terminal.Green("osmedeus cloud create --provider aws -n 3") + `  Create 3 AWS instances
  ` + terminal.Green("osmedeus cloud ls") + `                         List active infrastructure
  ` + terminal.Green("osmedeus cloud run -f general -t target.com") + ` Run scan on cloud workers
  ` + terminal.Green("osmedeus cloud destroy <id>") + `               Destroy specific infrastructure
  ` + terminal.Green("osmedeus cloud destroy all --force") + `        Destroy all infrastructure
`,
}

// cloudConfigCmd manages cloud configuration
var cloudConfigCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage cloud configuration",
	Long:  `View and update cloud configuration settings`,
}

// cloudConfigSetCmd sets a cloud config value
var cloudConfigSetCmd = &cobra.Command{
	Use:   "set [<key> <value>]",
	Short: "Set a cloud configuration value",
	Long: terminal.BoldCyan("◆ Description") + `
  Set a cloud configuration value using dot notation.

` + terminal.BoldCyan("▷ Examples") + `
  ` + terminal.Green("osmedeus cloud config set defaults.provider digitalocean") + `
  ` + terminal.Green("osmedeus cloud config set ssh.user ubuntu") + `

  ` + terminal.Green("# Batch set from a file") + `
  osmedeus cloud config set ` + terminal.Yellow("--from-file") + ` cloud-config.txt

  ` + terminal.Green("# Pipe from stdin") + `
  cat cloud-config.txt | osmedeus cloud config set ` + terminal.Yellow("--from-file") + ` -

` + terminal.BoldCyan("▷ File Format") + `
  Lines can use any of these formats:
    ssh.user "ubuntu"
    ssh.user = "ubuntu"
    osmedeus cloud config set ssh.user "ubuntu"
  Lines starting with # are ignored.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.Get()
		if cfg == nil {
			return errConfigNotLoaded
		}

		configPath := cfg.Cloud.CloudSettings
		if configPath == "" {
			configPath = filepath.Join(cfg.BaseFolder, "cloud", "cloud-settings.yaml")
		}

		// Auto-create from preset if file doesn't exist
		if _, err := os.Stat(configPath); os.IsNotExist(err) {
			if err := ensureCloudConfig(configPath); err != nil {
				return err
			}
		}

		// Load existing config
		cloudCfg, err := cloud.LoadCloudConfig(configPath)
		if err != nil {
			return fmt.Errorf("failed to load cloud config: %w", err)
		}

		// Resolve key-value pairs from args, file, or stdin
		pairs, err := resolveConfigSetPairs(args, cloudConfigSetFromFile)
		if err != nil {
			return err
		}

		var setErrors []string
		for _, pair := range pairs {
			key, value := pair[0], pair[1]

			if err := setCloudConfigValue(cloudCfg, key, value); err != nil {
				setErrors = append(setErrors, fmt.Sprintf("failed to set %s: %v", key, err))
				continue
			}

			// Save after each successful set to keep config consistent
			if err := cloud.SaveCloudConfig(cloudCfg, configPath); err != nil {
				setErrors = append(setErrors, fmt.Sprintf("failed to save after setting %s: %v", key, err))
				continue
			}

			printer.Success("Cloud config updated: %s = %s", terminal.Cyan(key), terminal.Green(redactValueForDisplay(key, value, false)))
		}

		if len(setErrors) > 0 {
			return fmt.Errorf("errors setting cloud config:\n  %s", strings.Join(setErrors, "\n  "))
		}
		return nil
	},
}

var cloudConfigListShowSecrets bool
var cloudConfigCleanForce bool

// cloudConfigListCmd lists cloud configuration as flattened key=value pairs
var cloudConfigListCmd = &cobra.Command{
	Use:     "list [filter]",
	Aliases: []string{"ls", "show"},
	Short:   "List cloud configuration values",
	Long:    `Display cloud configuration as flattened key=value pairs, optionally filtered by a substring match on key or value`,
	Args:    cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.Get()
		if cfg == nil {
			return errConfigNotLoaded
		}

		configPath := cfg.Cloud.CloudSettings
		if configPath == "" {
			configPath = filepath.Join(cfg.BaseFolder, "cloud", "cloud-settings.yaml")
		}

		// Auto-create from preset if file doesn't exist
		if _, err := os.Stat(configPath); os.IsNotExist(err) {
			if err := ensureCloudConfig(configPath); err != nil {
				return err
			}
		}

		// Read and parse YAML via AST to flatten
		content, err := os.ReadFile(configPath)
		if err != nil {
			return fmt.Errorf("failed to read cloud config: %w", err)
		}

		file, err := parser.ParseBytes(content, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("failed to parse cloud config: %w", err)
		}
		if len(file.Docs) == 0 {
			return fmt.Errorf("empty cloud config file")
		}

		out := map[string]string{}
		flattenASTScalars(file.Docs[0].Body, "", out)

		keys := make([]string, 0, len(out))
		for k := range out {
			keys = append(keys, k)
		}
		sortStrings(keys)

		// Apply fuzzy filter if provided
		filter := ""
		if len(args) > 0 {
			filter = strings.ToLower(args[0])
		}

		if globalJSON {
			result := make(map[string]string)
			for _, k := range keys {
				v := out[k]
				if !cloudConfigListShowSecrets {
					v = redactValueForDisplay(k, v, false)
				}
				if filter != "" {
					if !strings.Contains(strings.ToLower(k), filter) && !strings.Contains(strings.ToLower(v), filter) {
						continue
					}
				}
				result[k] = v
			}
			jsonBytes, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				return fmt.Errorf("failed to marshal cloud config: %w", err)
			}
			fmt.Println(string(jsonBytes))
			return nil
		}

		for _, k := range keys {
			v := out[k]
			if !cloudConfigListShowSecrets {
				v = redactValueForDisplay(k, v, false)
			}

			// Fuzzy filter: match on key or value (case-insensitive)
			if filter != "" {
				if !strings.Contains(strings.ToLower(k), filter) && !strings.Contains(strings.ToLower(v), filter) {
					continue
				}
			}

			fmt.Printf("%s = %s\n", getCategoryColor(k)(k), v)
		}
		return nil
	},
}


// cloudConfigCleanCmd resets cloud config and state to a fresh preset
var cloudConfigCleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Clean cloud configuration and generate a fresh one from preset",
	Long: terminal.BoldCyan("◆ Description") + `
  Remove the current cloud configuration and state, then generate a
  fresh cloud-settings.yaml from the built-in preset template.

  By default this command asks for confirmation. Use --force to skip.

` + terminal.BoldCyan("▷ Examples") + `
  ` + terminal.Green("osmedeus cloud config clean") + `
  ` + terminal.Green("osmedeus cloud config clean --force") + `
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.Get()
		if cfg == nil {
			return errConfigNotLoaded
		}

		configPath := cfg.Cloud.CloudSettings
		if configPath == "" {
			configPath = filepath.Join(cfg.BaseFolder, "cloud", "cloud-settings.yaml")
		}

		statePath := filepath.Join(cfg.BaseFolder, "cloud-state")

		// Check if there is anything to clean
		configExists := false
		if _, err := os.Stat(configPath); err == nil {
			configExists = true
		}
		stateExists := false
		if _, err := os.Stat(statePath); err == nil {
			stateExists = true
		}

		if !configExists && !stateExists {
			printer.Info("No cloud configuration or state found — nothing to clean")
			printer.Info("Generating fresh cloud config from preset")
			return ensureCloudConfig(configPath)
		}

		// Confirm unless --force
		if !cloudConfigCleanForce {
			printer.Warning("This will remove the following:")
			if configExists {
				printer.Bullet(configPath)
			}
			if stateExists {
				printer.Bullet(statePath + " (all infrastructure state)")
			}
			_, _ = fmt.Fprint(os.Stdout, "\n  Type 'yes' to continue: ")

			scanner := bufio.NewScanner(os.Stdin)
			scanner.Scan()
			if strings.TrimSpace(scanner.Text()) != "yes" {
				printer.Info("Aborted")
				return nil
			}
		}

		// Backup existing cloud config
		if configExists {
			backupPath := configPath + ".backup"
			if err := copyFile(configPath, backupPath); err != nil {
				printer.Warning("Could not backup config: %v", err)
			} else {
				printer.Info("Backed up existing config to %s", backupPath)
			}
			if err := os.Remove(configPath); err != nil {
				return fmt.Errorf("failed to remove cloud config: %w", err)
			}
			printer.Info("Removed cloud config: %s", configPath)
		}

		// Remove cloud state directory
		if stateExists {
			if err := os.RemoveAll(statePath); err != nil {
				return fmt.Errorf("failed to remove cloud state: %w", err)
			}
			printer.Info("Removed cloud state: %s", statePath)
		}

		// Generate fresh config from preset
		if err := ensureCloudConfig(configPath); err != nil {
			return err
		}

		printer.Success("Cloud configuration has been reset to defaults")
		return nil
	},
}

// copyFile copies src to dst, creating or overwriting dst
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	_, err = io.Copy(out, in)
	return err
}

// ensureCloudConfig creates the cloud config file from the embedded preset
func ensureCloudConfig(configPath string) error {
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return fmt.Errorf("failed to create cloud config directory: %w", err)
	}

	data, err := public.GetCloudConfigExample()
	if err != nil {
		return fmt.Errorf("failed to read embedded cloud config preset: %w", err)
	}

	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write cloud config: %w", err)
	}

	printer.Success("Created cloud config from preset at %s", configPath)

	// Also copy ansible playbook and inventory example to cloud-infra/
	baseFolder := filepath.Dir(filepath.Dir(configPath))
	ensureCloudInfraPresets(baseFolder)

	return nil
}

// cloudCreateCmd provisions cloud infrastructure
var cloudCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create cloud infrastructure",
	Long: terminal.BoldCyan("◆ Description") + `
  Provision cloud VMs. Instances stay running until you destroy them.

` + terminal.BoldCyan("▷ Examples") + `
  ` + terminal.Green("osmedeus cloud create --provider aws --instances 1") + `
  ` + terminal.Green("osmedeus cloud create --provider digitalocean -n 3") + `
  ` + terminal.Green("osmedeus cloud create --provider hetzner --instances 2") + `
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.Get()
		if cfg == nil {
			return errConfigNotLoaded
		}

		if !cfg.Cloud.Enabled {
			return fmt.Errorf("cloud features are disabled. Enable in osm-settings.yaml: cloud.enabled = true")
		}

		// Load cloud config
		cloudCfg, err := cloud.LoadCloudConfig(cfg.Cloud.CloudSettings)
		if err != nil {
			return fmt.Errorf("failed to load cloud config: %w", err)
		}
		cloud.ResolveTemplatePaths(cloudCfg, cfg.BaseFolder)

		// Override provider if specified
		providerType := cloud.ProviderType(cloudCfg.Defaults.Provider)
		if cloudProvider != "" {
			providerType = cloud.ProviderType(cloudProvider)
		}

		// Override mode if specified
		mode := cloud.ExecutionMode(cloudCfg.Defaults.Mode)
		if cloudMode != "" {
			mode = cloud.ExecutionMode(cloudMode)
		}

		// Override instance count if specified
		instanceCount := 1 // default to 1 instance unless explicitly specified
		if cloudInstances > 0 {
			instanceCount = cloudInstances
		}

		// Validate against limits
		if instanceCount > cloudCfg.Limits.MaxInstances {
			return fmt.Errorf("instance count (%d) exceeds limit (%d)", instanceCount, cloudCfg.Limits.MaxInstances)
		}

		printer.Section("Creating Cloud Infrastructure")
		printer.KeyValueColored("Provider", string(providerType), terminal.BoldCyan)
		printer.KeyValue("Mode", string(mode))
		printer.KeyValue("Instances", fmt.Sprintf("%d", instanceCount))

		// Create provider
		provider, err := cloud.CreateProvider(cloudCfg, providerType)
		if err != nil {
			return fmt.Errorf("failed to create provider: %w", err)
		}

		// Validate provider credentials
		ctx := context.Background()
		if err := provider.Validate(ctx); err != nil {
			return fmt.Errorf("provider validation failed: %w", err)
		}

		// Estimate cost
		estimate, err := provider.EstimateCost(mode, instanceCount)
		if err != nil {
			return fmt.Errorf("failed to estimate cost: %w", err)
		}

		printer.KeyValueColored("Est. cost", fmt.Sprintf("$%.2f/hour ($%.2f/day)", estimate.HourlyCost, estimate.DailyCost), terminal.Yellow)
		for _, note := range estimate.Notes {
			printer.Bullet(terminal.Gray(note))
		}

		// Read SSH public key
		sshPublicKey := ""
		if cloudCfg.SSH.PublicKeyContent != "" {
			sshPublicKey = cloudCfg.SSH.PublicKeyContent
		} else if cloudCfg.SSH.PublicKeyPath != "" {
			pubKeyPath := cloudCfg.SSH.PublicKeyPath
			if len(pubKeyPath) > 1 && pubKeyPath[:2] == "~/" {
				home, _ := os.UserHomeDir()
				pubKeyPath = filepath.Join(home, pubKeyPath[2:])
			}
			data, err := os.ReadFile(pubKeyPath)
			if err != nil {
				return fmt.Errorf("failed to read SSH public key: %w", err)
			}
			sshPublicKey = strings.TrimSpace(string(data))
		}

		// Create lifecycle manager
		lm := cloud.NewLifecycleManager(cloudCfg, provider, nil)

		// Create infrastructure
		createOpts := &cloud.CreateOptions{
			Mode:          mode,
			InstanceCount: instanceCount,
			SSHPublicKey:  sshPublicKey,
			SetupCommands: cloudCfg.Setup.Commands,
			Tags:          map[string]string{"managed-by": "osmedeus"},
		}

		printer.Newline()
		infra, err := lm.CreateAndRun(ctx, createOpts)
		if err != nil {
			return fmt.Errorf("failed to create infrastructure: %w", err)
		}

		printer.Newline()
		printer.Success("Infrastructure created: %s", terminal.BoldGreen(infra.ID))
		printer.Divider()
		for _, res := range infra.Resources {
			statusColor := terminal.Green
			if res.Status != "active" && res.Status != "running" {
				statusColor = terminal.Yellow
			}
			printer.KeyValueColored(res.Name, fmt.Sprintf("%s  %s", terminal.Bold(res.PublicIP), statusColor(res.Status)), terminal.Cyan)
		}
		printer.Divider()

		// Setup workers unless --skip-setup is specified
		if !cloudSkipSetup {
			sshAuth := cloudSSHAuthFromConfig(cloudCfg)
			if sshAuth.User == "" {
				sshAuth.User = "root"
			}

			// Use provider-specific SSH user from infra metadata
			if u, ok := infra.Metadata["ssh_user"].(string); ok && u != "" {
				sshAuth.User = u
			}

			// Wait for SSH on all workers
			printer.Section("Preparing Workers")
			var readyWorkers []cloud.Resource
			for _, res := range infra.Resources {
				if res.PublicIP == "" {
					printer.Warning("Skipping %s: no public IP", res.Name)
					continue
				}
				printer.Info("Waiting for SSH on %s (%s)...", res.Name, terminal.Cyan(res.PublicIP))
				if waitErr := waitForSSHPort(res.PublicIP, sshAuth.Port, 3*time.Minute); waitErr != nil {
					printer.Warning("SSH not ready on %s: %v", res.Name, waitErr)
					continue
				}
				printer.Success("SSH ready on %s", terminal.Cyan(res.PublicIP))
				readyWorkers = append(readyWorkers, res)
			}

			if len(readyWorkers) == 0 {
				printer.Warning("No workers reachable via SSH — skipping setup")
			} else {
				// Run setup commands (ansible or raw SSH)
				if cloudCfg.Setup.Ansible.Enabled || cloudUseAnsible {
					printer.Section("Running Ansible Setup")
					ensureCloudInfraPresets(cfg.BaseFolder)
					if ansibleErr := runAnsibleSetup(&cloudCfg.Setup.Ansible, readyWorkers, sshAuth); ansibleErr != nil {
						printer.Warning("Ansible setup failed: %v", ansibleErr)
						printer.Info("Falling back to SSH-based setup commands...")
						for _, res := range readyWorkers {
							if err := setupWorkerViaSSHAuth(sshAuth, res.PublicIP, cloudCfg.Setup.Commands); err != nil {
								printer.Warning("Setup failed on %s: %v", res.PublicIP, err)
							}
						}
					}
				} else {
					for _, res := range readyWorkers {
						if setupErr := setupWorkerViaSSHAuth(sshAuth, res.PublicIP, cloudCfg.Setup.Commands); setupErr != nil {
							printer.Warning("Setup failed on %s: %v", res.Name, setupErr)
						}
					}
				}

				// Run post-setup commands per-worker (with template vars)
				if len(cloudCfg.Setup.PostCommands) > 0 {
					for i, res := range readyWorkers {
						postVars := map[string]string{
							"public_ip":   res.PublicIP,
							"private_ip":  res.PrivateIP,
							"worker_name": res.Name,
							"worker_id":   res.ID,
							"infra_id":    infra.ID,
							"provider":    string(infra.Provider),
							"ssh_user":    sshAuth.User,
							"index":       fmt.Sprintf("%d", i),
						}
						runPostCommandsAuth(sshAuth, res.PublicIP, cloudCfg.Setup.PostCommands, postVars, cloudVerboseSetup)
					}
				}

				printer.Success("All workers set up and ready")
			}
		}

		printer.Info("Destroy when done: %s", terminal.Gray(fmt.Sprintf("osmedeus cloud destroy %s", infra.ID)))

		return nil
	},
}

// cloudListCmd lists cloud infrastructure
var cloudListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List cloud infrastructure",
	Long:    `List all active cloud infrastructure`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.Get()
		if cfg == nil {
			return errConfigNotLoaded
		}

		// Load cloud config to get the state path
		cloudCfg, err := cloud.LoadCloudConfig(cfg.Cloud.CloudSettings)
		statePath := cfg.Cloud.CloudPath
		if err == nil {
			cloud.ResolveTemplatePaths(cloudCfg, cfg.BaseFolder)
			statePath = cloudCfg.State.Path
		}
		infrastructures, err := cloud.ListInfrastructures(statePath)
		if err != nil {
			return fmt.Errorf("failed to list infrastructures: %w", err)
		}

		if len(infrastructures) == 0 {
			if globalJSON {
				fmt.Println("[]")
				return nil
			}
			printer.Info("No active cloud infrastructure found")
			return nil
		}

		// Build flat rows: one row per resource (VM)
		var rows []map[string]interface{}
		for _, infra := range infrastructures {
			if len(infra.Resources) == 0 {
				rows = append(rows, map[string]interface{}{
					"infra_id":  infra.ID,
					"provider":  string(infra.Provider),
					"status":    "no resources",
					"name":      "",
					"public_ip": "",
					"worker_id": "",
					"created":   infra.CreatedAt.Format("2006-01-02 15:04"),
				})
				continue
			}
			for _, res := range infra.Resources {
				rows = append(rows, map[string]interface{}{
					"infra_id":  infra.ID,
					"provider":  string(infra.Provider),
					"status":    res.Status,
					"name":      res.Name,
					"public_ip": res.PublicIP,
					"worker_id": res.WorkerID,
					"created":   infra.CreatedAt.Format("2006-01-02 15:04"),
				})
			}
		}

		if globalJSON {
			if rows == nil {
				fmt.Println("[]")
				return nil
			}
			jsonBytes, err := json.MarshalIndent(rows, "", "  ")
			if err != nil {
				return fmt.Errorf("failed to marshal infrastructure: %w", err)
			}
			fmt.Println(string(jsonBytes))
			return nil
		}

		columns := []string{"infra_id", "provider", "name", "public_ip", "status", "created"}
		renderTableWithTablewriter("cloud", rows, columns, 0, false, nil)

		return nil
	},
}

// cloudDestroyCmd destroys cloud infrastructure
var cloudDestroyCmd = &cobra.Command{
	Use:   "destroy [infrastructure-id]",
	Short: "Destroy cloud infrastructure",
	Long: terminal.BoldCyan("◆ Description") + `
  Tear down cloud infrastructure and clean up resources.

` + terminal.BoldCyan("▷ Examples") + `
  # Destroy a specific infrastructure
  ` + terminal.Green("osmedeus cloud destroy cloud-aws-1775159841") + `

  # Destroy all infrastructure (requires --force)
  ` + terminal.Green("osmedeus cloud destroy all --force") + `

  # List available infrastructure IDs
  ` + terminal.Green("osmedeus cloud ls") + `
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.Get()
		if cfg == nil {
			return errConfigNotLoaded
		}

		// Load cloud config to get the state path
		cloudCfg, loadErr := cloud.LoadCloudConfig(cfg.Cloud.CloudSettings)
		statePath := cfg.Cloud.CloudPath
		if loadErr == nil {
			cloud.ResolveTemplatePaths(cloudCfg, cfg.BaseFolder)
			statePath = cloudCfg.State.Path
		}

		// Helper to destroy a single infrastructure
		destroyOne := func(infra *cloud.Infrastructure) error {
			destroyCfg, err := cloud.LoadCloudConfig(cfg.Cloud.CloudSettings)
			if err != nil {
				return fmt.Errorf("failed to load cloud config: %w", err)
			}
			cloud.ResolveTemplatePaths(destroyCfg, cfg.BaseFolder)

			provider, err := cloud.CreateProvider(destroyCfg, infra.Provider)
			if err != nil {
				return fmt.Errorf("failed to create provider: %w", err)
			}

			printer.Info("Tearing down %s (%s)...", terminal.Cyan(infra.ID), terminal.Gray(string(infra.Provider)))
			lm := cloud.NewLifecycleManager(destroyCfg, provider, nil)
			if destroyErr := lm.Destroy(context.Background(), infra); destroyErr != nil {
				return destroyErr
			}
			printer.Success("Destroyed %s", terminal.BoldGreen(infra.ID))
			return nil
		}

		// "destroy all" — destroy everything
		if len(args) > 0 && args[0] == "all" {
			if !cloudForce {
				return fmt.Errorf("refusing to destroy all infrastructure without --force flag")
			}

			infrastructures, err := cloud.ListInfrastructures(statePath)
			if err != nil {
				return fmt.Errorf("failed to list infrastructures: %w", err)
			}
			if len(infrastructures) == 0 {
				printer.Info("No active infrastructure found")
				return nil
			}

			printer.Section(fmt.Sprintf("Destroying All Infrastructure (%d)", len(infrastructures)))
			for _, infra := range infrastructures {
				if err := destroyOne(infra); err != nil {
					printer.Warning("Failed to destroy %s: %v", infra.ID, err)
				}
			}
			printer.Success("All infrastructure destroyed")
			return nil
		}

		// Destroy specific infrastructure by ID
		if len(args) > 0 {
			infraID := args[0]
			infra, err := cloud.LoadInfrastructureState(infraID, statePath)
			if err != nil {
				return fmt.Errorf("failed to load infrastructure %s: %w", infraID, err)
			}

			printer.Section(fmt.Sprintf("Destroying Infrastructure: %s", terminal.BoldRed(infraID)))
			if err := destroyOne(infra); err != nil {
				return fmt.Errorf("failed to destroy infrastructure: %w", err)
			}
			return nil
		}

		// No ID: list available
		infrastructures, err := cloud.ListInfrastructures(statePath)
		if err != nil {
			return fmt.Errorf("failed to list infrastructures: %w", err)
		}

		if len(infrastructures) == 0 {
			printer.Info("No active infrastructure found")
			return nil
		}

		printer.Warning("Please specify an infrastructure ID to destroy:")
		for _, infra := range infrastructures {
			printer.Info("  %s (%s, %d resources)", infra.ID, infra.Provider, len(infra.Resources))
		}
		return nil
	},
}

// cloudRunCmd runs a workflow on cloud infrastructure
var cloudRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run workflow on cloud infrastructure",
	Long: terminal.BoldCyan("◆ Description") + `
  Provision cloud VMs, run setup commands, execute an osmedeus workflow,
  and stream output back to your terminal.

` + terminal.BoldCyan("▷ Examples") + `
  # Run a flow on a new AWS instance
  ` + terminal.Green("osmedeus cloud run -f fast -t example.com --provider aws --instances 1") + `

  # Run a module with a timeout
  ` + terminal.Green("osmedeus cloud run -m enum-subdomain -t example.com --timeout 30m") + `

  # Run on multiple instances
  ` + terminal.Green("osmedeus cloud run -f general -t example.com --provider aws --instances 3") + `

  # Auto-destroy infrastructure after scan completes
  ` + terminal.Green("osmedeus cloud run -f fast -t example.com --auto-destroy") + `

  # Reuse existing infrastructure (auto-discover from saved state)
  ` + terminal.Green("osmedeus cloud run -f fast -t example.com --reuse") + `

  # Reuse specific instances by IP
  ` + terminal.Green("osmedeus cloud run -f fast -t example.com --reuse-with '1.2.3.4,5.6.7.8'") + `

  # Show full setup command output
  ` + terminal.Green("osmedeus cloud run -f fast -t example.com --verbose-setup") + `

  # Run with targets from a file
  ` + terminal.Green("osmedeus cloud run -f domain-list-recon -T targets.txt --instances 2") + `

  # Sync results back to local machine after scan
  ` + terminal.Green("osmedeus cloud run -f fast -t example.com --sync-back") + `

  # Full lifecycle: provision, scan, sync results, destroy
  ` + terminal.Green("osmedeus cloud run -f fast -t example.com --sync-back --auto-destroy") + `

  # Combine: reuse infra + sync + auto-destroy
  ` + terminal.Green("osmedeus cloud run -f fast -t example.com --reuse --sync-back --auto-destroy") + `

` + terminal.BoldCyan("▷ Custom Command Mode") + `
  # Run custom commands on cloud workers (no osmedeus workflow)
  ` + terminal.Green("osmedeus cloud run --custom-cmd 'nmap -sV {{Target}} -oA /tmp/osm-custom/nmap' -t example.com") + `

  # Multiple commands with post-processing and sync-back
  ` + terminal.Green(`osmedeus cloud run \
    --custom-cmd 'nuclei -u {{Target}} -o /tmp/osm-custom/nuclei.txt' \
    --custom-post-cmd 'cat /tmp/osm-custom/nuclei.txt | notify' \
    --sync-path '/tmp/osm-custom/' \
    -t example.com`) + `

  # Distribute targets across workers with custom commands
  ` + terminal.Green(`osmedeus cloud run \
    --custom-cmd 'cat {{Target}} | httpx -o /tmp/osm-custom/live.txt' \
    --sync-path '/tmp/osm-custom/live.txt' \
    --sync-dest './my-results' \
    -T targets.txt --instances 3`) + `

  # Run on existing infrastructure
  ` + terminal.Green("osmedeus cloud run --custom-cmd 'whoami && id' -t example.com --reuse") + `
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.Get()
		if cfg == nil {
			return errConfigNotLoaded
		}

		if !cfg.Cloud.Enabled {
			return fmt.Errorf("cloud features are disabled. Enable in osm-settings.yaml: cloud.enabled = true")
		}

		// Validate workflow flags
		isCustomMode := len(cloudCustomCmds) > 0
		isFlowMode := cloudFlowName != "" || cloudModuleName != ""

		if isCustomMode && isFlowMode {
			return fmt.Errorf("--custom-cmd is mutually exclusive with --flow (-f) and --module (-m)")
		}
		if !isCustomMode && !isFlowMode {
			return fmt.Errorf("either --flow (-f), --module (-m), or --custom-cmd is required")
		}
		if cloudTarget == "" && cloudTargetFile == "" {
			return fmt.Errorf("either --target (-t) or --target-file (-T) is required")
		}
		if !isCustomMode && (len(cloudCustomPostCmds) > 0 || len(cloudSyncPaths) > 0) {
			return fmt.Errorf("--custom-post-cmd and --sync-path require --custom-cmd")
		}

		// Load cloud config
		cloudCfg, err := cloud.LoadCloudConfig(cfg.Cloud.CloudSettings)
		if err != nil {
			return fmt.Errorf("failed to load cloud config: %w", err)
		}
		cloud.ResolveTemplatePaths(cloudCfg, cfg.BaseFolder)

		// Override provider if specified
		providerType := cloud.ProviderType(cloudCfg.Defaults.Provider)
		if cloudProvider != "" {
			providerType = cloud.ProviderType(cloudProvider)
		}

		instanceCount := 1 // default to 1 instance unless explicitly specified
		if cloudInstances > 0 {
			instanceCount = cloudInstances
		}

		// Create provider
		provider, err := cloud.CreateProvider(cloudCfg, providerType)
		if err != nil {
			return fmt.Errorf("failed to create provider: %w", err)
		}

		// Validate provider credentials
		ctx := context.Background()
		if err := provider.Validate(ctx); err != nil {
			return fmt.Errorf("provider validation failed: %w", err)
		}

		// Read SSH keys
		sshPublicKey := ""
		if cloudCfg.SSH.PublicKeyContent != "" {
			sshPublicKey = cloudCfg.SSH.PublicKeyContent
		} else if cloudCfg.SSH.PublicKeyPath != "" {
			pubKeyPath := cloudCfg.SSH.PublicKeyPath
			if len(pubKeyPath) > 1 && pubKeyPath[:2] == "~/" {
				home, _ := os.UserHomeDir()
				pubKeyPath = filepath.Join(home, pubKeyPath[2:])
			}
			data, readErr := os.ReadFile(pubKeyPath)
			if readErr != nil {
				return fmt.Errorf("failed to read SSH public key: %w", readErr)
			}
			sshPublicKey = strings.TrimSpace(string(data))
		}

		sshAuth := cloudSSHAuthFromConfig(cloudCfg)
		sshUser := sshAuth.User
		if sshUser == "" {
			sshUser = "root"
			sshAuth.User = sshUser
		}

		// Step 1: Provision or reuse infrastructure
		var infra *cloud.Infrastructure

		statePath := cfg.Cloud.CloudPath
		if cloudCfg.State.Path != "" {
			statePath = cloudCfg.State.Path
		}

		if cloudReuseInfra && cloudReuseWith != "" {
			return fmt.Errorf("--reuse and --reuse-with are mutually exclusive")
		}

		if cloudReuseWith != "" {
			// Reuse specific instances by IP or name
			printer.Section("Reusing Specified Instances")
			identifiers := strings.Split(cloudReuseWith, ",")
			for i := range identifiers {
				identifiers[i] = strings.TrimSpace(identifiers[i])
			}

			infra = resolveReuseWithInstances(identifiers, statePath, sshAuth)
			printer.Success("Reusing %d instance(s)", len(infra.Resources))
			for _, res := range infra.Resources {
				printer.KeyValueColored(res.Name, res.PublicIP, terminal.Cyan)
			}
		} else if cloudReuseInfra {
			// Auto-discover all saved infrastructures
			printer.Section("Discovering Existing Infrastructure")
			discoveredInfra, discoverErr := discoverAndPrioritizeInfra(statePath, sshAuth)
			if discoverErr != nil {
				return discoverErr
			}
			infra = discoveredInfra
			printer.Success("Discovered %d reachable instance(s)", len(infra.Resources))
			for _, res := range infra.Resources {
				printer.KeyValueColored(res.Name, res.PublicIP, terminal.Cyan)
			}
		} else {
			// Provision new infrastructure
			printer.Section("Provisioning Cloud Infrastructure")
			printer.KeyValueColored("Provider", string(providerType), terminal.BoldCyan)
			printer.KeyValue("Instances", fmt.Sprintf("%d", instanceCount))

			lm := cloud.NewLifecycleManager(cloudCfg, provider, nil)
			createOpts := &cloud.CreateOptions{
				Mode:          cloud.ModeVM,
				InstanceCount: instanceCount,
				SSHPublicKey:  sshPublicKey,
				SetupCommands: cloudCfg.Setup.Commands,
				Tags:          map[string]string{"managed-by": "osmedeus"},
			}

			newInfra, createErr := lm.CreateAndRun(ctx, createOpts)
			if createErr != nil {
				return fmt.Errorf("failed to create infrastructure: %w", createErr)
			}
			infra = newInfra
			printer.Newline()
			printer.Success("Infrastructure ready: %s", terminal.BoldGreen(infra.ID))
		}

		// Use provider-specific SSH user from infra metadata
		if u, ok := infra.Metadata["ssh_user"].(string); ok && u != "" {
			sshUser = u
		}

		// Step 2: Wait for SSH on all workers
		printer.Section("Preparing Workers")
		var readyWorkers []cloud.Resource
		for _, res := range infra.Resources {
			if res.PublicIP == "" {
				printer.Warning("Skipping %s: no public IP", res.Name)
				continue
			}
			printer.Info("Waiting for SSH on %s (%s)...", res.Name, terminal.Cyan(res.PublicIP))
			if waitErr := waitForSSHPort(res.PublicIP, sshAuth.Port, 3*time.Minute); waitErr != nil {
				printer.Warning("SSH not ready on %s: %v", res.Name, waitErr)
				continue
			}
			printer.Success("SSH ready on %s", terminal.Cyan(res.PublicIP))
			readyWorkers = append(readyWorkers, res)
		}

		if len(readyWorkers) == 0 {
			return fmt.Errorf("no workers are reachable via SSH")
		}

		// Step 4: Setup workers (ansible or raw commands)
		if cloudCfg.Setup.Ansible.Enabled || cloudUseAnsible {
			// Ansible-based setup: run playbook against all workers at once
			printer.Section("Running Ansible Setup")
			ensureCloudInfraPresets(cfg.BaseFolder)
			if ansibleErr := runAnsibleSetup(&cloudCfg.Setup.Ansible, readyWorkers, sshAuth); ansibleErr != nil {
				printer.Warning("Ansible setup failed: %v", ansibleErr)
				printer.Info("Falling back to SSH-based setup commands...")
				for _, res := range readyWorkers {
					if err := setupWorkerViaSSHAuth(sshAuth, res.PublicIP, cloudCfg.Setup.Commands); err != nil {
						printer.Warning("Setup failed on %s: %v", res.PublicIP, err)
					}
				}
			}
		} else {
			// SSH-based setup: run commands per-worker
			for _, res := range readyWorkers {
				if setupErr := setupWorkerViaSSHAuth(sshAuth, res.PublicIP, cloudCfg.Setup.Commands); setupErr != nil {
					printer.Warning("Setup failed on %s: %v", res.Name, setupErr)
				}
			}
		}

		// Step 5: Run post-setup commands per-worker (with template vars)
		if len(cloudCfg.Setup.PostCommands) > 0 {
			for i, res := range readyWorkers {
				postVars := map[string]string{
					"public_ip":   res.PublicIP,
					"private_ip":  res.PrivateIP,
					"worker_name": res.Name,
					"worker_id":   res.ID,
					"infra_id":    infra.ID,
					"provider":    string(infra.Provider),
					"ssh_user":    sshUser,
					"index":       fmt.Sprintf("%d", i),
				}
				runPostCommandsAuth(sshAuth, res.PublicIP, cloudCfg.Setup.PostCommands, postVars, cloudVerboseSetup)
			}
		}

		// Branch: custom command mode vs flow/module mode
		if isCustomMode {
			// Custom command mode: run arbitrary commands on workers
			tasks, scanErrors := executeCustomCommands(ctx, sshAuth, sshUser, readyWorkers, infra, cloudCfg)

			// Print summary
			failCount := 0
			for _, e := range scanErrors {
				if e != nil {
					failCount++
				}
			}
			if failCount > 0 {
				printer.Warning("%d of %d workers failed", failCount, len(tasks))
			}

			// Sync custom paths back if requested
			if len(cloudSyncPaths) > 0 {
				syncCustomPaths(sshAuth, readyWorkers, infra, sshUser, cloudSyncPaths, cloudSyncDest)
			}
		} else {
			// Initialize database for cloud run tracking (best-effort)
			cloudRunGroupID := uuid.New().String()
			cloudDBReady := false
			if _, dbErr := database.Connect(cfg); dbErr == nil {
				if migErr := database.Migrate(ctx); migErr == nil {
					cloudDBReady = true
				}
			}

			// Step 5.5: Build per-worker commands and distribute target files
			var tasks []workerTask

			// Build the base command prefix (flow or module)
			var baseCmd string
			if cloudFlowName != "" {
				baseCmd = fmt.Sprintf("osmedeus run -f %s", cloudFlowName)
			} else {
				baseCmd = fmt.Sprintf("osmedeus run -m %s", cloudModuleName)
			}
			if cloudTimeout != "" {
				baseCmd += fmt.Sprintf(" --timeout %s", cloudTimeout)
			}

			if cloudTargetFile != "" {
				// Read targets locally and distribute across workers
				allTargets, readErr := readTargetsFromFile(cloudTargetFile)
				if readErr != nil {
					return fmt.Errorf("failed to read target file %s: %w", cloudTargetFile, readErr)
				}
				if len(allTargets) == 0 {
					return fmt.Errorf("target file %s is empty", cloudTargetFile)
				}

				// Validate chunk flags
				if cloudChunkSize > 0 && cloudChunkCount > 0 {
					return fmt.Errorf("--chunk-size and --chunk-count are mutually exclusive")
				}

				effectiveWorkers := len(readyWorkers)

				// Apply chunk overrides
				if cloudChunkCount > 0 {
					if cloudChunkCount > len(readyWorkers) {
						return fmt.Errorf("--chunk-count %d exceeds available workers %d", cloudChunkCount, len(readyWorkers))
					}
					effectiveWorkers = cloudChunkCount
				} else if cloudChunkSize > 0 {
					needed := (len(allTargets) + cloudChunkSize - 1) / cloudChunkSize
					if needed > len(readyWorkers) {
						printer.Warning("chunk-size %d requires %d workers but only %d available; using %d workers",
							cloudChunkSize, needed, len(readyWorkers), len(readyWorkers))
						effectiveWorkers = len(readyWorkers)
					} else {
						effectiveWorkers = needed
					}
				}

				// Cap workers at target count
				if len(allTargets) < effectiveWorkers {
					printer.Warning("Only %d targets for %d workers; using %d workers",
						len(allTargets), effectiveWorkers, len(allTargets))
					effectiveWorkers = len(allTargets)
				}

				chunks := splitTargetsForWorkers(allTargets, effectiveWorkers)

				printer.Section("Distributing Targets")
				printer.Info("Total targets: %d, Workers: %d", len(allTargets), effectiveWorkers)
				for i := 0; i < effectiveWorkers; i++ {
					chunk := chunks[i]
					if len(chunk) == 0 {
						continue
					}

					worker := readyWorkers[i]

					// Write chunk to local temp file
					uid := uuid.New().String()[:8]
					localTmp := filepath.Join(os.TempDir(), fmt.Sprintf("osm-cloud-targets-%s-%d.txt", uid, i))
					if writeErr := os.WriteFile(localTmp, []byte(strings.Join(chunk, "\n")+"\n"), 0644); writeErr != nil {
						return fmt.Errorf("failed to write temp target file: %w", writeErr)
					}

					// SCP to remote worker
					remotePath := fmt.Sprintf("/tmp/osm-targets-%d.txt", i)
					printer.Info("Uploading %d targets to %s (%s)", len(chunk), worker.Name, terminal.Cyan(worker.PublicIP))
					if scpErr := scpFileToRemote(sshAuth.KeyPath, sshAuth.User, worker.PublicIP, localTmp, remotePath); scpErr != nil {
						_ = os.Remove(localTmp)
						return fmt.Errorf("failed to SCP targets to %s: %w", worker.Name, scpErr)
					}
					_ = os.Remove(localTmp)

					// Build per-worker command with the remote target file
					workerCmd := fmt.Sprintf("%s -T %s", baseCmd, remotePath)

					tasks = append(tasks, workerTask{
						resource:  worker,
						osmCmd:    workerCmd,
						chunkInfo: fmt.Sprintf("%d targets", len(chunk)),
						targets:   chunk,
					})
				}
			} else {
				// Single target (-t): same command for all workers
				singleCmd := fmt.Sprintf("%s -t %s", baseCmd, cloudTarget)
				for _, res := range readyWorkers {
					tasks = append(tasks, workerTask{
						resource: res,
						osmCmd:   singleCmd,
						targets:  []string{cloudTarget},
					})
				}
			}

			// Step 6: Run scans in parallel
			printer.Section("Starting Scans")
			pathSetup := "export PATH=$HOME/.local/bin:$HOME/osmedeus-base/external-binaries:$HOME/go/bin:/usr/local/go/bin:$PATH"

			var wg sync.WaitGroup
			scanErrors := make([]error, len(tasks))

			for i, task := range tasks {
				wg.Add(1)
				go func(idx int, t workerTask) {
					defer wg.Done()
					label := t.resource.Name
					if t.chunkInfo != "" {
						printer.Info("[%s] %s (%s@%s): %s",
							label, t.chunkInfo, terminal.Cyan(sshUser), terminal.Bold(t.resource.PublicIP), terminal.Gray(t.osmCmd))
					} else {
						printer.Info("[%s] (%s@%s): %s",
							label, terminal.Cyan(sshUser), terminal.Bold(t.resource.PublicIP), terminal.Gray(t.osmCmd))
					}

					// Create a Run record for this worker (best-effort)
					var workerRunUUID string
					if cloudDBReady {
						now := time.Now()
						workerRunUUID = uuid.New().String()

						workerTarget := cloudTarget
						if len(t.targets) == 1 {
							workerTarget = t.targets[0]
						} else if len(t.targets) > 1 {
							workerTarget = fmt.Sprintf("%d targets", len(t.targets))
						}

						workflowName := cloudFlowName
						workflowKind := "flow"
						if cloudModuleName != "" {
							workflowName = cloudModuleName
							workflowKind = "module"
						}

						run := &database.Run{
							RunUUID:      workerRunUUID,
							WorkflowName: workflowName,
							WorkflowKind: workflowKind,
							Target:       workerTarget,
							Params: map[string]interface{}{
								"cloud_provider": string(providerType),
								"cloud_infra_id": infra.ID,
								"worker_name":    t.resource.Name,
								"worker_ip":      t.resource.PublicIP,
								"osm_command":    t.osmCmd,
							},
							Status:      "running",
							TriggerType: "cli",
							RunGroupID:  cloudRunGroupID,
							StartedAt:   &now,
							Workspace:   computeWorkspace(workerTarget, map[string]string{}),
							RunPriority: "critical",
							RunMode:     "cloud",
						}

						if createErr := database.CreateRun(ctx, run); createErr != nil {
							workerRunUUID = ""
						}
					}

					// Build progress callback for step tracking
					var onLine cloud.LineCallback
					if cloudDBReady && workerRunUUID != "" {
						onLine = newCloudProgressParser(ctx, workerRunUUID)
					}

					scanCmd := fmt.Sprintf("%s && %s", pathSetup, t.osmCmd)
					var scanErr error
					if onLine != nil {
						scanErr = runSSHCommandStreamingAuthWithCallback(sshAuth, t.resource.PublicIP, scanCmd, onLine, t.resource.Name)
					} else {
						scanErr = runSSHCommandStreamingAuth(sshAuth, t.resource.PublicIP, scanCmd, t.resource.Name)
					}

					if scanErr != nil {
						scanErrors[idx] = scanErr
						printer.Warning("Scan failed on %s: %v", t.resource.Name, scanErr)
					} else {
						printer.Success("Scan completed on %s", t.resource.Name)
					}

					// Update run record status (best-effort)
					if cloudDBReady && workerRunUUID != "" {
						if scanErr != nil {
							_ = database.UpdateRunStatus(ctx, workerRunUUID, "failed", scanErr.Error())
						} else {
							_ = database.UpdateRunStatus(ctx, workerRunUUID, "completed", "")
						}
					}
				}(i, task)
			}

			wg.Wait()

			// Print summary
			failCount := 0
			for _, e := range scanErrors {
				if e != nil {
					failCount++
				}
			}
			if failCount > 0 {
				printer.Warning("%d of %d workers failed", failCount, len(tasks))
			}

			// Sync results back from workers if requested
			if cloudSyncBack {
				printer.Section("Syncing Results Back")
				for _, task := range tasks {
					for _, target := range task.targets {
						p := terminal.NewPrinter()
						p.Info("Syncing %s from %s...", terminal.Bold(target), terminal.Cyan(task.resource.PublicIP))
						if syncErr := syncWorkspaceBack(sshAuth, task.resource.PublicIP, target, cfg); syncErr != nil {
							printer.Warning("  Sync failed for %s: %v", target, syncErr)
						}
					}
				}
				printer.Success("All results synced to local workspace")
			}
		}

		// Auto-destroy infrastructure if requested
		if cloudAutoDestroy {
			printer.Section("Destroying Infrastructure")
			printer.Info("Auto-destroying %s...", terminal.Cyan(infra.ID))
			destroyCfg, loadErr := cloud.LoadCloudConfig(cfg.Cloud.CloudSettings)
			if loadErr == nil {
				cloud.ResolveTemplatePaths(destroyCfg, cfg.BaseFolder)
				destroyProvider, provErr := cloud.CreateProvider(destroyCfg, infra.Provider)
				if provErr == nil {
					lm := cloud.NewLifecycleManager(destroyCfg, destroyProvider, nil)
					if destroyErr := lm.Destroy(ctx, infra); destroyErr != nil {
						printer.Warning("Failed to destroy: %v", destroyErr)
						printer.Warning("Manual cleanup: osmedeus cloud destroy %s", infra.ID)
					} else {
						printer.Success("Infrastructure %s destroyed", terminal.BoldGreen(infra.ID))
					}
				}
			}
		} else {
			printer.Section("Cloud Run Summary")
			printer.Divider()
			printer.KeyValueColored("Infrastructure", infra.ID, terminal.BoldGreen)
			printer.KeyValue("Workers", fmt.Sprintf("%d", len(infra.Resources)))
			for _, res := range infra.Resources {
				printer.KeyValueColored(res.Name, res.PublicIP, terminal.Cyan)
			}
			printer.Divider()
			printer.Newline()
			printer.Bullet(fmt.Sprintf("Destroy:  %s", terminal.Gray(fmt.Sprintf("osmedeus cloud destroy %s", infra.ID))))
		}

		return nil
	},
}

// cloudSSHAuth holds SSH authentication context for cloud commands.
// It bridges cloud config to the Go-native SSH client in internal/cloud/ssh.go.
type cloudSSHAuth struct {
	KeyPath  string
	Password string
	User     string
	Port     string
}

func cloudSSHAuthFromConfig(cfg *config.CloudConfigs) cloudSSHAuth {
	return cloudSSHAuth{
		KeyPath:  cloud.ExpandPath(cfg.SSH.PrivateKeyPath),
		Password: cfg.SSH.Password,
		User:     cfg.SSH.User,
		Port:     cfg.SSH.Port,
	}
}

// toSSHConfig converts to cloud.SSHConfig for use with CloudSSHClient
func (a cloudSSHAuth) toSSHConfig(host string) cloud.SSHConfig {
	port := 22
	if a.Port != "" {
		if p, err := fmt.Sscanf(a.Port, "%d", &port); p == 0 || err != nil {
			port = 22
		}
	}
	return cloud.SSHConfig{
		Host:     host,
		Port:     port,
		User:     a.User,
		KeyFile:  a.KeyPath,
		Password: a.Password,
	}
}

// connect creates a Go-native SSH client for the given host
func (a cloudSSHAuth) connect(ctx context.Context, host string) (*cloud.CloudSSHClient, error) {
	return cloud.NewCloudSSHClient(ctx, a.toSSHConfig(host))
}

// runSSHCommandAuth runs a command via Go-native SSH and returns output
func runSSHCommandAuth(auth cloudSSHAuth, host, command string) (string, error) {
	ctx := context.Background()
	client, err := auth.connect(ctx, host)
	if err != nil {
		return "", err
	}
	defer client.Close()
	out, _, runErr := client.RunCommand(ctx, command)
	return out, runErr
}

// runSSHCommandStreamingAuth runs a command via Go-native SSH, streaming output with prefix
func runSSHCommandStreamingAuth(auth cloudSSHAuth, host, command string, prefixLabel ...string) error {
	ctx := context.Background()
	client, err := auth.connect(ctx, host)
	if err != nil {
		return err
	}
	defer client.Close()
	label := "remote"
	if len(prefixLabel) > 0 && prefixLabel[0] != "" {
		label = prefixLabel[0]
	}
	return client.RunCommandStreaming(ctx, command, label)
}

// runSSHCommandStreamingAuthWithCallback runs a command via Go-native SSH, streaming output
// with prefix and calling onLine for each output line for progress tracking.
func runSSHCommandStreamingAuthWithCallback(auth cloudSSHAuth, host, command string, onLine cloud.LineCallback, prefixLabel ...string) error {
	ctx := context.Background()
	client, err := auth.connect(ctx, host)
	if err != nil {
		return err
	}
	defer client.Close()
	label := "remote"
	if len(prefixLabel) > 0 && prefixLabel[0] != "" {
		label = prefixLabel[0]
	}
	return client.RunCommandStreamingWithCallback(ctx, command, label, onLine)
}

// newCloudProgressParser returns a callback that parses structured log lines from remote
// osmedeus output and increments the completed_steps counter for the given run.
func newCloudProgressParser(ctx context.Context, runUUID string) func(string) {
	return func(line string) {
		idx := strings.Index(line, "{")
		if idx < 0 {
			return
		}
		var entry struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal([]byte(line[idx:]), &entry) != nil {
			return
		}
		if entry.Msg == "Step completed" {
			_ = database.IncrementRunCompletedSteps(ctx, runUUID)
		}
	}
}

// uploadFileAuth copies a local file to remote via Go-native SFTP
func uploadFileAuth(auth cloudSSHAuth, host, localPath, remotePath string) error {
	ctx := context.Background()
	client, err := auth.connect(ctx, host)
	if err != nil {
		return err
	}
	defer client.Close()
	return client.UploadFile(localPath, remotePath)
}

func scpFileToRemote(keyPath, user, host, localPath, remotePath string) error {
	return uploadFileAuth(cloudSSHAuth{KeyPath: keyPath, User: user}, host, localPath, remotePath)
}

// isOsmedeusRunning checks if an osmedeus process is running on a remote host.
// Returns true if busy, false if idle. Errors are treated as unreachable.
func isOsmedeusRunning(auth cloudSSHAuth, host string) (bool, error) {
	out, err := runSSHCommandAuth(auth, host, "pgrep -f 'osmedeus run|osmedeus cloud' || true")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// discoverAndPrioritizeInfra loads all saved infrastructures, checks SSH reachability,
// and prioritizes idle instances (no osmedeus process running).
func discoverAndPrioritizeInfra(statePath string, auth cloudSSHAuth) (*cloud.Infrastructure, error) {
	allInfras, err := cloud.ListInfrastructures(statePath)
	if err != nil {
		return nil, fmt.Errorf("failed to list infrastructures: %w", err)
	}
	if len(allInfras) == 0 {
		return nil, fmt.Errorf("no saved infrastructure found. Provision first with: osmedeus cloud run -f <flow> -t <target>")
	}

	// Collect all resources with public IPs across all infrastructures
	type resourceInfo struct {
		resource  cloud.Resource
		reachable bool
		idle      bool
	}
	var candidates []resourceInfo

	for _, inf := range allInfras {
		for _, res := range inf.Resources {
			if res.PublicIP == "" {
				continue
			}
			candidates = append(candidates, resourceInfo{resource: res})
		}
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no instances with public IPs found in saved infrastructure")
	}

	// Check reachability and busyness in parallel
	type checkResult struct {
		idx       int
		reachable bool
		idle      bool
	}
	results := make(chan checkResult, len(candidates))

	for i, c := range candidates {
		go func(idx int, res cloud.Resource) {
			// Quick SSH port check (15s timeout)
			if waitErr := waitForSSHPort(res.PublicIP, auth.Port, 15*time.Second); waitErr != nil {
				printer.Warning("Skipping %s (%s): unreachable", res.Name, res.PublicIP)
				results <- checkResult{idx: idx, reachable: false}
				return
			}
			busy, err := isOsmedeusRunning(auth, res.PublicIP)
			if err != nil {
				printer.Warning("Skipping %s (%s): SSH error: %v", res.Name, res.PublicIP, err)
				results <- checkResult{idx: idx, reachable: false}
				return
			}
			if busy {
				printer.Info("Instance %s (%s) is busy (osmedeus running)", res.Name, terminal.Yellow(res.PublicIP))
			} else {
				printer.Info("Instance %s (%s) is idle", res.Name, terminal.Green(res.PublicIP))
			}
			results <- checkResult{idx: idx, reachable: true, idle: !busy}
		}(i, c.resource)
	}

	// Collect results
	for range candidates {
		r := <-results
		candidates[r.idx].reachable = r.reachable
		candidates[r.idx].idle = r.idle
	}

	// Build merged infrastructure: idle instances first, then busy ones
	var idleResources, busyResources []cloud.Resource
	for _, c := range candidates {
		if !c.reachable {
			continue
		}
		if c.idle {
			idleResources = append(idleResources, c.resource)
		} else {
			busyResources = append(busyResources, c.resource)
		}
	}

	allReady := append(idleResources, busyResources...)
	if len(allReady) == 0 {
		return nil, fmt.Errorf("no reachable instances found in saved infrastructure")
	}

	return &cloud.Infrastructure{
		ID:        "reuse-discovered",
		Resources: allReady,
		Metadata:  allInfras[0].Metadata, // inherit metadata from first infra for SSH user etc.
	}, nil
}

// resolveReuseWithInstances resolves comma-separated IPs/names against saved state,
// falling back to ad-hoc resources for unrecognized identifiers.
func resolveReuseWithInstances(identifiers []string, statePath string, auth cloudSSHAuth) *cloud.Infrastructure {
	// Load all saved infras to match against
	allInfras, _ := cloud.ListInfrastructures(statePath)

	// Build lookup maps from saved state
	ipToResource := make(map[string]cloud.Resource)
	nameToResource := make(map[string]cloud.Resource)
	for _, inf := range allInfras {
		for _, res := range inf.Resources {
			if res.PublicIP != "" {
				ipToResource[res.PublicIP] = res
			}
			if res.Name != "" {
				nameToResource[res.Name] = res
			}
		}
	}

	var resources []cloud.Resource
	seen := make(map[string]bool)

	for _, id := range identifiers {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true

		// Try matching by IP first, then by name
		if res, ok := ipToResource[id]; ok {
			printer.Info("Matched %s from saved state (%s)", terminal.Cyan(id), res.Name)
			resources = append(resources, res)
		} else if res, ok := nameToResource[id]; ok {
			printer.Info("Matched %s from saved state (%s)", terminal.Cyan(id), res.PublicIP)
			resources = append(resources, res)
		} else {
			// Treat as ad-hoc IP
			printer.Info("Using %s as ad-hoc instance", terminal.Yellow(id))
			resources = append(resources, cloud.Resource{
				Name:     fmt.Sprintf("adhoc-%s", id),
				PublicIP: id,
				Type:     "vm",
				Status:   "running",
			})
		}
	}

	// Inherit metadata from first saved infra if available
	var metadata map[string]interface{}
	if len(allInfras) > 0 {
		metadata = allInfras[0].Metadata
	}

	return &cloud.Infrastructure{
		ID:        "reuse-specified",
		Resources: resources,
		Metadata:  metadata,
	}
}

// splitTargetsForWorkers divides targets into contiguous chunks, one per worker.
func splitTargetsForWorkers(allTargets []string, workerCount int) [][]string {
	total := len(allTargets)
	if total == 0 || workerCount <= 0 {
		return nil
	}
	chunkSize := (total + workerCount - 1) / workerCount
	chunks := make([][]string, workerCount)
	for i := 0; i < workerCount; i++ {
		start := i * chunkSize
		if start >= total {
			break
		}
		end := start + chunkSize
		if end > total {
			end = total
		}
		chunks[i] = allTargets[start:end]
	}
	return chunks
}

// workerTask holds the per-worker command and metadata for parallel scan execution.
type workerTask struct {
	resource  cloud.Resource
	osmCmd    string
	chunkInfo string   // e.g. "5 targets"
	targets   []string // targets assigned to this worker (for sync-back)
}

func setupWorkerViaSSHAuth(auth cloudSSHAuth, host string, commands []string) error {
	p := terminal.NewPrinter()

	// Filter out comments and empty lines
	var cmds []string
	for _, cmd := range commands {
		if strings.TrimSpace(cmd) == "" || strings.HasPrefix(strings.TrimSpace(cmd), "#") {
			continue
		}
		cmds = append(cmds, cmd)
	}

	if len(cmds) == 0 {
		p.Info("No setup commands configured — skipping worker setup")
		p.Info("Configure via: %s", terminal.Gray("osmedeus cloud config set setup.commands.add \"<command>\""))
		return nil
	}

	p.Info("Running %d setup commands on %s...", len(cmds), terminal.Cyan(host))

	// PATH prefix so osmedeus and tools are found in non-interactive shells
	envPrefix := "export DEBIAN_FRONTEND=noninteractive && export PATH=$HOME/.local/bin:$HOME/osmedeus-base/external-binaries:$HOME/go/bin:/usr/local/go/bin:$PATH"

	for i, cmd := range cmds {
		p.Info("  [%d/%d] %s %s", i+1, len(cmds), terminal.Gray("$"), terminal.Cyan(cmd))
		fullCmd := fmt.Sprintf("%s && %s", envPrefix, cmd)
		if cloudVerboseSetup {
			if err := runSSHCommandStreamingAuth(auth, host, fullCmd); err != nil {
				p.Warning("  Command failed (%v)", err)
			}
		} else {
			if _, err := runSSHCommandAuth(auth, host, fullCmd); err != nil {
				p.Warning("  Command failed (%v)", err)
			}
		}
	}

	p.Success("Setup complete on %s", terminal.Cyan(host))
	return nil
}

// expandPostCommandVars replaces template variables in a post-command string
func expandPostCommandVars(cmd string, vars map[string]string) string {
	for k, v := range vars {
		cmd = strings.ReplaceAll(cmd, "{{"+k+"}}", v)
	}
	return cmd
}

func runPostCommandsAuth(auth cloudSSHAuth, host string, commands []string, vars map[string]string, verbose bool) {
	p := terminal.NewPrinter()

	var cmds []string
	for _, cmd := range commands {
		if strings.TrimSpace(cmd) == "" || strings.HasPrefix(strings.TrimSpace(cmd), "#") {
			continue
		}
		cmds = append(cmds, cmd)
	}
	if len(cmds) == 0 {
		return
	}

	envPrefix := "export DEBIAN_FRONTEND=noninteractive && export PATH=$HOME/.local/bin:$HOME/osmedeus-base/external-binaries:$HOME/go/bin:/usr/local/go/bin:$PATH"

	p.Info("Running %d post-setup commands on %s...", len(cmds), terminal.Cyan(host))
	for i, cmd := range cmds {
		expanded := expandPostCommandVars(cmd, vars)
		p.Info("  [%d/%d] %s %s", i+1, len(cmds), terminal.Gray("$"), terminal.Cyan(expanded))
		fullCmd := fmt.Sprintf("%s && %s", envPrefix, expanded)
		if verbose {
			if err := runSSHCommandStreamingAuth(auth, host, fullCmd); err != nil {
				p.Warning("  Post-command failed (%v)", err)
			}
		} else {
			if _, err := runSSHCommandAuth(auth, host, fullCmd); err != nil {
				p.Warning("  Post-command failed (%v)", err)
			}
		}
	}
	p.Success("Post-setup complete on %s", terminal.Cyan(host))
}

// executeCustomCommands runs custom commands on all workers in parallel.
// Each worker runs --custom-cmd commands sequentially, stopping on first failure.
// If all custom-cmds succeed, --custom-post-cmd commands run in order.
func executeCustomCommands(
	ctx context.Context,
	sshAuth cloudSSHAuth,
	sshUser string,
	readyWorkers []cloud.Resource,
	infra *cloud.Infrastructure,
	cloudCfg *config.CloudConfigs,
) ([]workerTask, []error) {
	printer := terminal.NewPrinter()

	// --- Target distribution ---
	type customWorkerCtx struct {
		resource  cloud.Resource
		targetVal string // value for {{Target}}
		index     int
	}
	var workerCtxs []customWorkerCtx

	if cloudTargetFile != "" {
		allTargets, readErr := readTargetsFromFile(cloudTargetFile)
		if readErr != nil {
			printer.Warning("Failed to read target file: %v", readErr)
			return nil, []error{readErr}
		}
		if len(allTargets) == 0 {
			printer.Warning("Target file %s is empty", cloudTargetFile)
			return nil, []error{fmt.Errorf("target file is empty")}
		}

		if cloudChunkSize > 0 && cloudChunkCount > 0 {
			return nil, []error{fmt.Errorf("--chunk-size and --chunk-count are mutually exclusive")}
		}

		effectiveWorkers := len(readyWorkers)
		if cloudChunkCount > 0 {
			if cloudChunkCount > len(readyWorkers) {
				effectiveWorkers = len(readyWorkers)
			} else {
				effectiveWorkers = cloudChunkCount
			}
		} else if cloudChunkSize > 0 {
			needed := (len(allTargets) + cloudChunkSize - 1) / cloudChunkSize
			if needed > len(readyWorkers) {
				effectiveWorkers = len(readyWorkers)
			} else {
				effectiveWorkers = needed
			}
		}
		if len(allTargets) < effectiveWorkers {
			effectiveWorkers = len(allTargets)
		}

		chunks := splitTargetsForWorkers(allTargets, effectiveWorkers)

		printer.Section("Distributing Targets")
		printer.Info("Total targets: %d, Workers: %d", len(allTargets), effectiveWorkers)
		for i := 0; i < effectiveWorkers; i++ {
			chunk := chunks[i]
			if len(chunk) == 0 {
				continue
			}

			worker := readyWorkers[i]
			uid := uuid.New().String()[:8]
			localTmp := filepath.Join(os.TempDir(), fmt.Sprintf("osm-cloud-targets-%s-%d.txt", uid, i))
			if writeErr := os.WriteFile(localTmp, []byte(strings.Join(chunk, "\n")+"\n"), 0644); writeErr != nil {
				printer.Warning("Failed to write temp target file: %v", writeErr)
				continue
			}
			remotePath := fmt.Sprintf("/tmp/osm-targets-%d.txt", i)
			printer.Info("Uploading %d targets to %s (%s)", len(chunk), worker.Name, terminal.Cyan(worker.PublicIP))
			if scpErr := scpFileToRemote(sshAuth.KeyPath, sshAuth.User, worker.PublicIP, localTmp, remotePath); scpErr != nil {
				_ = os.Remove(localTmp)
				printer.Warning("Failed to SCP targets to %s: %v", worker.Name, scpErr)
				continue
			}
			_ = os.Remove(localTmp)

			workerCtxs = append(workerCtxs, customWorkerCtx{
				resource:  worker,
				targetVal: remotePath,
				index:     i,
			})
		}
	} else {
		// Single target: same for all workers
		for i, res := range readyWorkers {
			workerCtxs = append(workerCtxs, customWorkerCtx{
				resource:  res,
				targetVal: cloudTarget,
				index:     i,
			})
		}
	}

	if len(workerCtxs) == 0 {
		printer.Warning("No workers to run custom commands on")
		return nil, nil
	}

	// --- Run commands in parallel across workers ---
	printer.Section("Running Custom Commands")

	tasks := make([]workerTask, len(workerCtxs))
	scanErrors := make([]error, len(workerCtxs))
	var wg sync.WaitGroup

	pathSetup := "export PATH=$HOME/.local/bin:$HOME/osmedeus-base/external-binaries:$HOME/go/bin:/usr/local/go/bin:$PATH"
	workdirSetup := "mkdir -p /tmp/osm-custom && cd /tmp/osm-custom"

	for i, wCtx := range workerCtxs {
		tasks[i] = workerTask{
			resource: wCtx.resource,
			targets:  []string{wCtx.targetVal},
		}

		wg.Add(1)
		go func(idx int, wc customWorkerCtx) {
			defer wg.Done()
			label := wc.resource.Name

			// Build template vars
			vars := map[string]string{
				"Target":      wc.targetVal,
				"public_ip":   wc.resource.PublicIP,
				"private_ip":  wc.resource.PrivateIP,
				"worker_name": wc.resource.Name,
				"worker_id":   wc.resource.ID,
				"infra_id":    infra.ID,
				"provider":    string(infra.Provider),
				"ssh_user":    sshUser,
				"index":       fmt.Sprintf("%d", wc.index),
			}

			// Run each --custom-cmd in order, stop on first failure
			cmdFailed := false
			for ci, rawCmd := range cloudCustomCmds {
				expanded := expandPostCommandVars(rawCmd, vars)
				printer.Info("[%s] custom-cmd [%d/%d]: %s", label, ci+1, len(cloudCustomCmds), terminal.Cyan(expanded))
				fullCmd := fmt.Sprintf("%s && %s && %s", pathSetup, workdirSetup, expanded)
				if err := runSSHCommandStreamingAuth(sshAuth, wc.resource.PublicIP, fullCmd, label); err != nil {
					scanErrors[idx] = fmt.Errorf("custom-cmd %d failed on %s: %w", ci+1, label, err)
					printer.Warning("[%s] custom-cmd %d failed: %v — skipping remaining commands", label, ci+1, err)
					cmdFailed = true
					break
				}
				printer.Success("[%s] custom-cmd %d completed", label, ci+1)
			}

			// Run --custom-post-cmd only if all custom-cmds succeeded
			if !cmdFailed && len(cloudCustomPostCmds) > 0 {
				for pi, rawCmd := range cloudCustomPostCmds {
					expanded := expandPostCommandVars(rawCmd, vars)
					printer.Info("[%s] post-cmd [%d/%d]: %s", label, pi+1, len(cloudCustomPostCmds), terminal.Cyan(expanded))
					fullCmd := fmt.Sprintf("%s && %s && %s", pathSetup, workdirSetup, expanded)
					if err := runSSHCommandStreamingAuth(sshAuth, wc.resource.PublicIP, fullCmd, label); err != nil {
						printer.Warning("[%s] post-cmd %d failed: %v", label, pi+1, err)
					} else {
						printer.Success("[%s] post-cmd %d completed", label, pi+1)
					}
				}
			}
		}(i, wCtx)
	}

	wg.Wait()
	return tasks, scanErrors
}

// syncCustomPaths downloads specified remote paths from each worker to local disk.
// Local layout: <syncDest>/<workerName>-<ip>/<relativePath>
func syncCustomPaths(
	sshAuth cloudSSHAuth,
	workers []cloud.Resource,
	infra *cloud.Infrastructure,
	sshUser string,
	syncPaths []string,
	syncDest string,
) {
	printer := terminal.NewPrinter()
	printer.Section("Syncing Custom Paths")

	ctx := context.Background()
	for i, res := range workers {
		workerDir := fmt.Sprintf("%s-%s", res.Name, res.PublicIP)
		localBase := filepath.Join(syncDest, workerDir)

		client, err := sshAuth.connect(ctx, res.PublicIP)
		if err != nil {
			printer.Warning("SSH connect failed for %s: %v", res.Name, err)
			continue
		}

		// Build template vars for expanding sync paths
		vars := map[string]string{
			"Target":      cloudTarget,
			"public_ip":   res.PublicIP,
			"private_ip":  res.PrivateIP,
			"worker_name": res.Name,
			"worker_id":   res.ID,
			"infra_id":    infra.ID,
			"provider":    string(infra.Provider),
			"ssh_user":    sshUser,
			"index":       fmt.Sprintf("%d", i),
		}

		for _, rawPath := range syncPaths {
			remotePath := expandPostCommandVars(rawPath, vars)

			// Determine local destination preserving remote path structure
			localPath := filepath.Join(localBase, remotePath)

			// Check if remote path is a file or directory
			checkCmd := fmt.Sprintf("test -d '%s' && echo DIR || (test -f '%s' && echo FILE || echo MISSING)", remotePath, remotePath)
			out, _, _ := client.RunCommand(ctx, checkCmd)
			pathType := strings.TrimSpace(out)

			switch pathType {
			case "DIR":
				printer.Info("Downloading dir %s from %s...", terminal.Bold(remotePath), terminal.Cyan(res.PublicIP))
				if err := client.DownloadDir(remotePath, localPath); err != nil {
					printer.Warning("Failed to download dir %s from %s: %v", remotePath, res.Name, err)
				} else {
					printer.Success("Downloaded %s → %s", remotePath, terminal.Gray(localPath))
				}
			case "FILE":
				printer.Info("Downloading file %s from %s...", terminal.Bold(remotePath), terminal.Cyan(res.PublicIP))
				if mkErr := os.MkdirAll(filepath.Dir(localPath), 0755); mkErr != nil {
					printer.Warning("Failed to create local dir for %s: %v", localPath, mkErr)
					continue
				}
				if err := client.DownloadFile(remotePath, localPath); err != nil {
					printer.Warning("Failed to download file %s from %s: %v", remotePath, res.Name, err)
				} else {
					printer.Success("Downloaded %s → %s", remotePath, terminal.Gray(localPath))
				}
			default:
				printer.Warning("Path %s not found on %s", remotePath, res.Name)
			}
		}

		client.Close()
	}

	printer.Success("Sync complete → %s", terminal.Bold(syncDest))
}

// runAnsibleSetup runs an ansible playbook against all workers.
// It generates a dynamic inventory file, then runs ansible-playbook locally.
func runAnsibleSetup(ansibleCfg *config.AnsibleSetup, workers []cloud.Resource, auth cloudSSHAuth) error {
	p := terminal.NewPrinter()

	// Check ansible-playbook is installed
	if _, err := execPkg.LookPath("ansible-playbook"); err != nil {
		return fmt.Errorf("ansible-playbook not found in PATH — install ansible first")
	}

	// Check playbook exists
	if _, err := os.Stat(ansibleCfg.PlaybookPath); os.IsNotExist(err) {
		return fmt.Errorf("playbook not found: %s", ansibleCfg.PlaybookPath)
	}

	// Generate inventory file
	inventoryPath := ansibleCfg.InventoryPath
	if err := os.MkdirAll(filepath.Dir(inventoryPath), 0755); err != nil {
		return fmt.Errorf("failed to create inventory directory: %w", err)
	}

	sshUser := auth.User
	if sshUser == "" {
		sshUser = "root"
	}

	var inv strings.Builder
	inv.WriteString("[osmedeus_workers]\n")
	for _, w := range workers {
		if w.PublicIP == "" {
			continue
		}
		hostLine := fmt.Sprintf("%s ansible_user=%s", w.PublicIP, sshUser)
		// Key-based auth
		if auth.KeyPath != "" {
			hostLine += fmt.Sprintf(" ansible_ssh_private_key_file=%s", auth.KeyPath)
		}
		// Password auth
		if auth.Password != "" {
			hostLine += fmt.Sprintf(" ansible_ssh_pass=%s", auth.Password)
		}
		// Custom port
		if auth.Port != "" && auth.Port != "22" {
			hostLine += fmt.Sprintf(" ansible_port=%s", auth.Port)
		}
		inv.WriteString(hostLine + "\n")
	}
	inv.WriteString("\n[osmedeus_workers:vars]\n")
	inv.WriteString("ansible_ssh_common_args='-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR'\n")

	if err := os.WriteFile(inventoryPath, []byte(inv.String()), 0644); err != nil {
		return fmt.Errorf("failed to write inventory: %w", err)
	}
	p.Info("Inventory written: %s (%d workers)", terminal.Gray(inventoryPath), len(workers))

	// Build ansible-playbook command
	args := []string{"-i", inventoryPath, ansibleCfg.PlaybookPath}

	// Add extra vars
	for k, v := range ansibleCfg.ExtraVars {
		args = append(args, "--extra-vars", fmt.Sprintf("%s=%s", k, v))
	}

	// Add extra args
	if ansibleCfg.ExtraArgs != "" {
		args = append(args, strings.Fields(ansibleCfg.ExtraArgs)...)
	}

	p.Info("Running: %s %s", terminal.Gray("$"), terminal.Cyan("ansible-playbook "+strings.Join(args, " ")))
	p.Divider()

	cmd := execPkg.Command("ansible-playbook", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ansible-playbook failed: %w", err)
	}

	p.Divider()
	p.Success("Ansible setup complete")
	return nil
}

// ensureCloudInfraPresets copies the default cloud-infra preset files if they don't exist
func ensureCloudInfraPresets(baseFolder string) {
	infraDir := filepath.Join(baseFolder, "cloud-infra")
	if err := os.MkdirAll(infraDir, 0755); err != nil {
		return
	}

	// Copy playbook if missing
	playbookPath := filepath.Join(infraDir, "setup-playbook.yaml")
	if _, err := os.Stat(playbookPath); os.IsNotExist(err) {
		data, readErr := public.EmbedFS.ReadFile("presets/cloud-infra/setup-playbook.yaml")
		if readErr == nil {
			_ = os.WriteFile(playbookPath, data, 0644)
		}
	}

	// Copy inventory example if missing
	examplePath := filepath.Join(infraDir, "inventory.ini.example")
	if _, err := os.Stat(examplePath); os.IsNotExist(err) {
		data, readErr := public.EmbedFS.ReadFile("presets/cloud-infra/inventory.ini.example")
		if readErr == nil {
			_ = os.WriteFile(examplePath, data, 0644)
		}
	}
}

// waitForSSH waits for SSH (port 22) to become reachable on the given host
// syncWorkspaceBack downloads a workspace from a remote worker and imports it locally.
// It runs `osmedeus snapshot export` on the remote, downloads the ZIP via SFTP,
// then imports using the existing snapshot import (which handles path differences and DB replay).
func syncWorkspaceBack(auth cloudSSHAuth, host, target string, cfg *config.Config) error {
	p := terminal.NewPrinter()

	ctx := context.Background()
	client, err := auth.connect(ctx, host)
	if err != nil {
		return fmt.Errorf("SSH connect failed: %w", err)
	}
	defer client.Close()

	pathSetup := "export PATH=$HOME/.local/bin:$HOME/osmedeus-base/external-binaries:$HOME/go/bin:/usr/local/go/bin:$PATH"
	remoteZip := fmt.Sprintf("/tmp/%s.zip", target)

	// Step 1: Export workspace on remote
	p.Info("  Exporting workspace on %s for target %s...", terminal.Cyan(host), terminal.Bold(target))
	exportCmd := fmt.Sprintf("%s && osmedeus snapshot export %s -o %s", pathSetup, target, remoteZip)
	out, exitCode, runErr := client.RunCommand(ctx, exportCmd)
	if runErr != nil || exitCode != 0 {
		return fmt.Errorf("remote snapshot export failed (exit %d): %s", exitCode, strings.TrimSpace(out))
	}

	// Step 2: Download ZIP via SFTP
	// Resolve the actual remote path (~ is expanded by the shell, but SFTP needs absolute)
	resolveCmd := fmt.Sprintf("echo %s", remoteZip)
	resolvedPath, _, _ := client.RunCommand(ctx, resolveCmd)
	resolvedPath = strings.TrimSpace(resolvedPath)
	if resolvedPath == "" {
		resolvedPath = remoteZip
	}

	localZip := filepath.Join(os.TempDir(), fmt.Sprintf("%s.zip", target))
	p.Info("  Downloading %s from %s...", terminal.Gray(target+".zip"), terminal.Cyan(host))
	if dlErr := client.DownloadFile(resolvedPath, localZip); dlErr != nil {
		return fmt.Errorf("SFTP download failed: %w", dlErr)
	}
	defer func() { _ = os.Remove(localZip) }()

	// Step 3: Import locally
	p.Info("  Importing workspace %s locally...", terminal.Bold(target))
	importResult, importErr := snapshot.ForceImportWorkspace(localZip, cfg.WorkspacesPath, false, cfg)
	if importErr != nil {
		return fmt.Errorf("local import failed: %w", importErr)
	}
	p.Success("  Imported %s → %s", terminal.Bold(target), terminal.Gray(importResult.LocalPath))

	// Step 4: Clean up remote ZIP
	_, _, _ = client.RunCommand(ctx, "rm -f "+remoteZip)

	return nil
}

func waitForSSHPort(host, port string, timeout time.Duration) error {
	if port == "" {
		port = "22"
	}
	addr := host + ":" + port
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("SSH not ready on %s after %s", addr, timeout)
}

// cloudSetupCmd sets up a remote machine without provisioning
var cloudSetupCmd = &cobra.Command{
	Use:   "setup <ip> [ip2] [ip3] ...",
	Short: "Setup osmedeus on existing remote machines",
	Long: terminal.BoldCyan("◆ Description") + `
  Run setup commands on existing remote machines via SSH.
  Uses the same SSH key, user, and setup.commands from cloud-settings.yaml
  but skips cloud provisioning. Useful for VMs you already have.

` + terminal.BoldCyan("▷ Examples") + `
  # Setup a single machine
  ` + terminal.Green("osmedeus cloud setup 1.2.3.4") + `

  # Setup multiple machines
  ` + terminal.Green("osmedeus cloud setup 1.2.3.4 5.6.7.8 9.10.11.12") + `

  # Show full setup output
  ` + terminal.Green("osmedeus cloud setup 1.2.3.4 --verbose-setup") + `

  # Use ansible playbook for setup
  ` + terminal.Green("osmedeus cloud setup 1.2.3.4 5.6.7.8 --ansible") + `

  # Then run a scan using the setup machines
  ` + terminal.Green("osmedeus cloud run -f fast -t example.com --reuse") + `
`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.Get()
		if cfg == nil {
			return errConfigNotLoaded
		}

		// Load cloud config
		configPath := cfg.Cloud.CloudSettings
		if _, err := os.Stat(configPath); os.IsNotExist(err) {
			if err := ensureCloudConfig(configPath); err != nil {
				return err
			}
		}
		cloudCfg, err := cloud.LoadCloudConfig(configPath)
		if err != nil {
			return fmt.Errorf("failed to load cloud config: %w", err)
		}
		cloud.ResolveTemplatePaths(cloudCfg, cfg.BaseFolder)

		// SSH config
		sshAuth := cloudSSHAuthFromConfig(cloudCfg)
		sshUser := sshAuth.User
		if sshUser == "" {
			sshUser = "root"
			sshAuth.User = sshUser
		}

		// Build worker list from args
		var workers []cloud.Resource
		for i, ip := range args {
			workers = append(workers, cloud.Resource{
				Type:     "vm",
				Name:     fmt.Sprintf("remote-%d", i),
				PublicIP: ip,
				Status:   "active",
			})
		}

		printer.Section("Setting Up Remote Machines")
		printer.KeyValue("Machines", fmt.Sprintf("%d", len(workers)))
		printer.KeyValue("SSH User", sshUser)
		for _, w := range workers {
			printer.Bullet(terminal.Cyan(w.PublicIP))
		}
		printer.Newline()

		// Wait for SSH on all
		var readyWorkers []cloud.Resource
		for _, w := range workers {
			printer.Info("Waiting for SSH on %s...", terminal.Cyan(w.PublicIP))
			if waitErr := waitForSSHPort(w.PublicIP, sshAuth.Port, 2*time.Minute); waitErr != nil {
				printer.Warning("SSH not ready on %s: %v", w.PublicIP, waitErr)
				continue
			}
			printer.Success("SSH ready on %s", terminal.Cyan(w.PublicIP))
			readyWorkers = append(readyWorkers, w)
		}

		if len(readyWorkers) == 0 {
			return fmt.Errorf("no machines are reachable via SSH")
		}

		// Run ansible or SSH commands
		if cloudCfg.Setup.Ansible.Enabled || cloudUseAnsible {
			printer.Section("Running Ansible Setup")
			ensureCloudInfraPresets(cfg.BaseFolder)
			if ansibleErr := runAnsibleSetup(&cloudCfg.Setup.Ansible, readyWorkers, sshAuth); ansibleErr != nil {
				printer.Warning("Ansible setup failed: %v", ansibleErr)
				printer.Info("Falling back to SSH-based setup commands...")
				for _, w := range readyWorkers {
					if err := setupWorkerViaSSHAuth(sshAuth, w.PublicIP, cloudCfg.Setup.Commands); err != nil {
						printer.Warning("Setup failed on %s: %v", w.PublicIP, err)
					}
				}
			}
		} else {
			for _, w := range readyWorkers {
				if setupErr := setupWorkerViaSSHAuth(sshAuth, w.PublicIP, cloudCfg.Setup.Commands); setupErr != nil {
					printer.Warning("Setup failed on %s: %v", w.PublicIP, setupErr)
				}
			}
		}

		// Run post-setup commands
		if len(cloudCfg.Setup.PostCommands) > 0 {
			for i, w := range readyWorkers {
				postVars := map[string]string{
					"public_ip":   w.PublicIP,
					"private_ip":  w.PrivateIP,
					"worker_name": w.Name,
					"worker_id":   w.ID,
					"infra_id":    "remote-adhoc",
					"provider":    "remote-adhoc",
					"ssh_user":    sshUser,
					"index":       fmt.Sprintf("%d", i),
				}
				runPostCommandsAuth(sshAuth, w.PublicIP, cloudCfg.Setup.PostCommands, postVars, cloudVerboseSetup)
			}
		}

		// Save as remote-adhoc infrastructure state so `cloud ls` and `cloud destroy` can see it
		infraID := fmt.Sprintf("remote-adhoc-%d", time.Now().Unix())
		infra := &cloud.Infrastructure{
			ID:        infraID,
			Provider:  "remote-adhoc",
			Mode:      cloud.ModeVM,
			CreatedAt: time.Now(),
			Resources: readyWorkers,
			Metadata: map[string]interface{}{
				"ssh_user": sshUser,
			},
		}
		if err := cloud.SaveInfrastructureState(infra, cloudCfg.State.Path); err != nil {
			printer.Warning("Failed to save state: %v", err)
		}

		printer.Newline()
		printer.Divider()
		printer.Success("Setup complete")
		printer.KeyValueColored("Infrastructure", infraID, terminal.BoldGreen)
		for _, w := range readyWorkers {
			printer.KeyValueColored(w.Name, w.PublicIP, terminal.Cyan)
		}
		printer.Divider()
		printer.Newline()
		printer.Info("Run a scan: %s", terminal.Gray("osmedeus cloud run -f fast -t example.com --reuse"))

		return nil
	},
}

// setCloudConfigValue sets a nested config value using dot notation
func setCloudConfigValue(cfg *config.CloudConfigs, key, value string) error {
	parts := strings.Split(key, ".")
	if len(parts) < 2 {
		return fmt.Errorf("invalid key format. Use dot notation (e.g., defaults.provider)")
	}

	// Simple implementation for common keys
	switch parts[0] {
	case "defaults":
		switch parts[1] {
		case "provider":
			cfg.Defaults.Provider = value
		case "mode":
			cfg.Defaults.Mode = value
		case "max_instances":
			var val int
			if _, err := fmt.Sscanf(value, "%d", &val); err != nil {
				return fmt.Errorf("invalid integer value: %s", value)
			}
			cfg.Defaults.MaxInstances = val
		case "use_spot":
			cfg.Defaults.UseSpot = (value == "true")
		case "timeout":
			cfg.Defaults.Timeout = value
		case "cleanup_on_failure":
			cfg.Defaults.CleanupOnFailure = (value == "true")
		default:
			return fmt.Errorf("unknown key: %s", key)
		}

	case "providers":
		if len(parts) < 3 {
			return fmt.Errorf("provider key requires 3 parts (e.g., providers.digitalocean.token)")
		}
		switch parts[1] {
		case "digitalocean":
			switch parts[2] {
			case "token":
				cfg.Providers.DigitalOcean.Token = value
			case "region":
				cfg.Providers.DigitalOcean.Region = value
			case "size":
				cfg.Providers.DigitalOcean.Size = value
			case "image":
				cfg.Providers.DigitalOcean.Image = value
			case "snapshot_id":
				cfg.Providers.DigitalOcean.SnapshotID = value
			case "ssh_key_id":
				cfg.Providers.DigitalOcean.SSHKeyID = value
			case "ssh_key_fingerprint":
				cfg.Providers.DigitalOcean.SSHKeyFingerprint = value
			default:
				return fmt.Errorf("unknown DigitalOcean key: %s", parts[2])
			}
		case "aws":
			switch parts[2] {
			case "access_key_id":
				cfg.Providers.AWS.AccessKeyID = value
			case "secret_access_key":
				cfg.Providers.AWS.SecretAccessKey = value
			case "region":
				cfg.Providers.AWS.Region = value
			case "instance_type":
				cfg.Providers.AWS.InstanceType = value
			case "ami":
				cfg.Providers.AWS.AMI = value
			case "ami_filter":
				cfg.Providers.AWS.AMIFilter = value
			case "use_spot":
				cfg.Providers.AWS.UseSpot = (value == "true")
			default:
				return fmt.Errorf("unknown AWS key: %s", parts[2])
			}
		case "gcp":
			switch parts[2] {
			case "project_id":
				cfg.Providers.GCP.ProjectID = value
			case "credentials_file":
				cfg.Providers.GCP.CredentialsFile = value
			case "region":
				cfg.Providers.GCP.Region = value
			case "zone":
				cfg.Providers.GCP.Zone = value
			case "machine_type":
				cfg.Providers.GCP.MachineType = value
			case "image_family":
				cfg.Providers.GCP.ImageFamily = value
			case "use_preemptible":
				cfg.Providers.GCP.UsePreemptible = (value == "true")
			default:
				return fmt.Errorf("unknown GCP key: %s", parts[2])
			}
		case "linode":
			switch parts[2] {
			case "token":
				cfg.Providers.Linode.Token = value
			case "region":
				cfg.Providers.Linode.Region = value
			case "type":
				cfg.Providers.Linode.Type = value
			case "image":
				cfg.Providers.Linode.Image = value
			case "ssh_public_key":
				cfg.Providers.Linode.SSHPublicKey = value
			default:
				return fmt.Errorf("unknown Linode key: %s", parts[2])
			}
		case "azure":
			switch parts[2] {
			case "subscription_id":
				cfg.Providers.Azure.SubscriptionID = value
			case "tenant_id":
				cfg.Providers.Azure.TenantID = value
			case "client_id":
				cfg.Providers.Azure.ClientID = value
			case "client_secret":
				cfg.Providers.Azure.ClientSecret = value
			case "location":
				cfg.Providers.Azure.Location = value
			case "vm_size":
				cfg.Providers.Azure.VMSize = value
			case "image_reference":
				cfg.Providers.Azure.ImageReference = value
			default:
				return fmt.Errorf("unknown Azure key: %s", parts[2])
			}
		case "hetzner":
			switch parts[2] {
			case "token":
				cfg.Providers.Hetzner.Token = value
			case "location":
				cfg.Providers.Hetzner.Location = value
			case "server_type":
				cfg.Providers.Hetzner.ServerType = value
			case "image":
				cfg.Providers.Hetzner.Image = value
			case "ssh_key_name":
				cfg.Providers.Hetzner.SSHKeyName = value
			default:
				return fmt.Errorf("unknown Hetzner key: %s", parts[2])
			}
		default:
			return fmt.Errorf("unknown provider: %s", parts[1])
		}

	case "limits":
		switch parts[1] {
		case "max_hourly_spend":
			var val float64
			if _, err := fmt.Sscanf(value, "%f", &val); err != nil {
				return fmt.Errorf("invalid float value: %s", value)
			}
			cfg.Limits.MaxHourlySpend = val
		case "max_total_spend":
			var val float64
			if _, err := fmt.Sscanf(value, "%f", &val); err != nil {
				return fmt.Errorf("invalid float value: %s", value)
			}
			cfg.Limits.MaxTotalSpend = val
		case "max_instances":
			var val int
			if _, err := fmt.Sscanf(value, "%d", &val); err != nil {
				return fmt.Errorf("invalid integer value: %s", value)
			}
			cfg.Limits.MaxInstances = val
		default:
			return fmt.Errorf("unknown limit key: %s", parts[1])
		}

	case "ssh":
		switch parts[1] {
		case "private_key_path":
			cfg.SSH.PrivateKeyPath = value
		case "private_key_content":
			cfg.SSH.PrivateKeyContent = value
		case "public_key_path":
			cfg.SSH.PublicKeyPath = value
		case "public_key_content":
			cfg.SSH.PublicKeyContent = value
		case "user":
			cfg.SSH.User = value
		case "password":
			cfg.SSH.Password = value
		case "port":
			cfg.SSH.Port = value
		default:
			return fmt.Errorf("unknown SSH key: %s", parts[1])
		}

	case "setup":
		switch parts[1] {
		case "commands":
			if len(parts) >= 3 {
				switch parts[2] {
				case "add":
					cfg.Setup.Commands = append(cfg.Setup.Commands, value)
					return nil
				case "clear":
					cfg.Setup.Commands = []string{}
					return nil
				}
			}
			cfg.Setup.Commands = []string{value}
		case "post_commands":
			if len(parts) >= 3 {
				switch parts[2] {
				case "add":
					cfg.Setup.PostCommands = append(cfg.Setup.PostCommands, value)
					return nil
				case "clear":
					cfg.Setup.PostCommands = []string{}
					return nil
				}
			}
			cfg.Setup.PostCommands = []string{value}
		case "ansible":
			if len(parts) < 3 {
				return fmt.Errorf("ansible key requires 3 parts (e.g., setup.ansible.enabled)")
			}
			switch parts[2] {
			case "enabled":
				cfg.Setup.Ansible.Enabled = (value == "true")
			case "playbook_path":
				cfg.Setup.Ansible.PlaybookPath = value
			case "inventory_path":
				cfg.Setup.Ansible.InventoryPath = value
			case "extra_args":
				cfg.Setup.Ansible.ExtraArgs = value
			default:
				// Handle extra_vars with 4-part keys: setup.ansible.extra_vars.<key>
				if len(parts) >= 4 && parts[2] == "extra_vars" {
					if cfg.Setup.Ansible.ExtraVars == nil {
						cfg.Setup.Ansible.ExtraVars = make(map[string]string)
					}
					cfg.Setup.Ansible.ExtraVars[parts[3]] = value
					return nil
				}
				return fmt.Errorf("unknown ansible key: %s", parts[2])
			}
		default:
			return fmt.Errorf("unknown setup key: %s. Use: setup.commands, setup.post_commands, setup.ansible", parts[1])
		}

	case "state":
		switch parts[1] {
		case "backend":
			cfg.State.Backend = value
		case "path":
			cfg.State.Path = value
		default:
			return fmt.Errorf("unknown state key: %s", parts[1])
		}

	default:
		return fmt.Errorf("unknown config section: %s", parts[0])
	}

	return nil
}

func init() {
	// Add subcommands
	cloudCmd.AddCommand(cloudConfigCmd)
	cloudCmd.AddCommand(cloudCreateCmd)
	cloudCmd.AddCommand(cloudListCmd)
	cloudCmd.AddCommand(cloudDestroyCmd)
	cloudCmd.AddCommand(cloudRunCmd)
	cloudCmd.AddCommand(cloudSetupCmd)

	cloudSetupCmd.Flags().BoolVar(&cloudVerboseSetup, "verbose-setup", false, "Show full setup output")
	cloudSetupCmd.Flags().BoolVar(&cloudUseAnsible, "ansible", false, "Use ansible playbook for setup (overrides config)")

	cloudConfigCmd.AddCommand(cloudConfigSetCmd)
	cloudConfigSetCmd.Flags().StringVar(&cloudConfigSetFromFile, "from-file", "", "Read key-value pairs from a file (or use - for stdin)")
	cloudConfigCmd.AddCommand(cloudConfigListCmd)
	cloudConfigCmd.AddCommand(cloudConfigCleanCmd)
	cloudConfigListCmd.Flags().BoolVar(&cloudConfigListShowSecrets, "show-secrets", false, "show sensitive values")
	cloudConfigCleanCmd.Flags().BoolVar(&cloudConfigCleanForce, "force", false, "skip confirmation prompt")

	// Flags for create command
	cloudCreateCmd.Flags().StringVarP(&cloudProvider, "provider", "p", "", "Cloud provider (aws, gcp, digitalocean, linode, azure, hetzner)")
	cloudCreateCmd.Flags().StringVarP(&cloudMode, "mode", "m", "", "Execution mode (vm, serverless)")
	cloudCreateCmd.Flags().IntVarP(&cloudInstances, "instances", "n", 0, "Number of instances to create")
	cloudCreateCmd.Flags().BoolVarP(&cloudForce, "force", "f", false, "Force recreation of existing infrastructure")
	cloudCreateCmd.Flags().BoolVar(&cloudSkipSetup, "skip-setup", false, "Skip worker setup after provisioning")
	cloudCreateCmd.Flags().BoolVar(&cloudVerboseSetup, "verbose-setup", false, "Show full setup output")
	cloudCreateCmd.Flags().BoolVar(&cloudUseAnsible, "ansible", false, "Use ansible playbook for setup (overrides config)")

	// Flags for destroy command
	cloudDestroyCmd.Flags().BoolVar(&cloudForce, "force", false, "Force destroy (required for 'destroy all')")

	// Flags for run command
	cloudRunCmd.Flags().StringVarP(&cloudFlowName, "flow", "f", "", "Flow workflow name to execute")
	cloudRunCmd.Flags().StringVarP(&cloudModuleName, "module", "m", "", "Module workflow name to execute")
	cloudRunCmd.Flags().StringVarP(&cloudTarget, "target", "t", "", "Target to scan")
	cloudRunCmd.Flags().StringVarP(&cloudTargetFile, "target-file", "T", "", "File containing targets")
	cloudRunCmd.Flags().StringVarP(&cloudProvider, "provider", "p", "", "Cloud provider")
	cloudRunCmd.Flags().IntVarP(&cloudInstances, "instances", "n", 0, "Number of instances")
	cloudRunCmd.Flags().StringVar(&cloudTimeout, "timeout", "", "Scan timeout (e.g., 2h, 30m)")
	cloudRunCmd.Flags().BoolVar(&cloudAutoDestroy, "auto-destroy", false, "Destroy infrastructure after scan completes")
	cloudRunCmd.Flags().BoolVar(&cloudReuseInfra, "reuse", false, "Auto-discover and reuse existing infrastructure (skip provisioning)")
	cloudRunCmd.Flags().StringVar(&cloudReuseWith, "reuse-with", "", "Reuse specific instances by public IP or name (comma-separated)")
	cloudRunCmd.Flags().BoolVar(&cloudVerboseSetup, "verbose-setup", false, "Show full setup output (default: quiet)")
	cloudRunCmd.Flags().BoolVar(&cloudUseAnsible, "ansible", false, "Use ansible playbook for setup (overrides config)")
	cloudRunCmd.Flags().IntVar(&cloudChunkSize, "chunk-size", 0, "Number of targets per chunk (mutually exclusive with --chunk-count)")
	cloudRunCmd.Flags().IntVar(&cloudChunkCount, "chunk-count", 0, "Split targets into N equal chunks (mutually exclusive with --chunk-size)")
	cloudRunCmd.Flags().BoolVar(&cloudSyncBack, "sync-back", false, "Download results and import into local database after scan")

	// Custom command mode flags
	cloudRunCmd.Flags().StringArrayVar(&cloudCustomCmds, "custom-cmd", nil, "Custom command to run on workers (repeatable, mutually exclusive with -f/-m)")
	cloudRunCmd.Flags().StringArrayVar(&cloudCustomPostCmds, "custom-post-cmd", nil, "Post-command to run after custom-cmds succeed (repeatable)")
	cloudRunCmd.Flags().StringArrayVar(&cloudSyncPaths, "sync-path", nil, "Remote file/dir to download after execution (repeatable)")
	cloudRunCmd.Flags().StringVar(&cloudSyncDest, "sync-dest", "./osm-sync-back", "Local base directory for synced files")
}

package cloud

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/pulumi/pulumi-gcp/sdk/v8/go/gcp/compute"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// GCPProvider implements the Provider interface for Google Cloud Platform
type GCPProvider struct {
	projectID       string
	credentialsFile string
	region          string
	zone            string
	machineType     string
	imageFamily     string
	usePreemptible  bool
}

// NewGCPProvider creates a new GCP provider
func NewGCPProvider(projectID, credentialsFile, region, zone, machineType, imageFamily string, usePreemptible bool) (*GCPProvider, error) {
	if projectID == "" {
		return nil, fmt.Errorf("GCP project ID is required")
	}

	if region == "" {
		region = "us-central1"
	}
	if zone == "" {
		zone = "us-central1-a"
	}
	if machineType == "" {
		machineType = "n1-standard-2"
	}
	if imageFamily == "" {
		imageFamily = "ubuntu-2204-lts"
	}

	return &GCPProvider{
		projectID:       projectID,
		credentialsFile: credentialsFile,
		region:          region,
		zone:            zone,
		machineType:     machineType,
		imageFamily:     imageFamily,
		usePreemptible:  usePreemptible,
	}, nil
}

// Validate checks if the provider configuration is valid
func (p *GCPProvider) Validate(ctx context.Context) error {
	if p.credentialsFile == "" {
		return fmt.Errorf("GCP credentials file path is required; set it via cloud config or GOOGLE_APPLICATION_CREDENTIALS")
	}

	if _, err := os.Stat(p.credentialsFile); os.IsNotExist(err) {
		return fmt.Errorf("GCP credentials file not found at %s", p.credentialsFile)
	}

	return nil
}

// EstimateCost estimates the cost for the given configuration
func (p *GCPProvider) EstimateCost(mode ExecutionMode, instanceCount int) (*CostEstimate, error) {
	if mode != ModeVM {
		return nil, fmt.Errorf("only VM mode is supported for GCP")
	}

	// Default pricing for common machine types in us-central1 (USD per hour)
	pricing := map[string]float64{
		"e2-micro":       0.0084,
		"e2-small":       0.0168,
		"e2-medium":      0.0335,
		"n1-standard-1":  0.0475,
		"n1-standard-2":  0.0950,
		"n1-standard-4":  0.1900,
		"n2-standard-2":  0.0971,
		"n2-standard-4":  0.1942,
		"c2-standard-4":  0.2088,
	}

	hourlyRate, ok := pricing[p.machineType]
	if !ok {
		// Default to n1-standard-2 pricing if unknown
		hourlyRate = 0.0950
	}

	notes := []string{
		fmt.Sprintf("%d x %s instances @ $%.4f/hr each", instanceCount, p.machineType, hourlyRate),
	}

	if p.usePreemptible {
		hourlyRate *= 0.20 // 80% discount for preemptible instances
		notes = append(notes, "preemptible instances: 80% discount applied")
	}

	totalHourlyRate := hourlyRate * float64(instanceCount)

	return &CostEstimate{
		HourlyCost: totalHourlyRate,
		DailyCost:  totalHourlyRate * 24,
		Currency:   "USD",
		Breakdown: map[string]float64{
			"compute": totalHourlyRate,
		},
		Notes: notes,
	}, nil
}

// CreateInfrastructure provisions GCP compute instances
func (p *GCPProvider) CreateInfrastructure(ctx context.Context, opts *CreateOptions) (*Infrastructure, error) {
	infraID := fmt.Sprintf("cloud-gcp-%d", time.Now().Unix())
	statePath := opts.StatePath

	// Set GOOGLE_CREDENTIALS env var from credentials file content
	credContent, err := os.ReadFile(p.credentialsFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read GCP credentials file: %w", err)
	}
	if err := os.Setenv("GOOGLE_CREDENTIALS", string(credContent)); err != nil {
		return nil, fmt.Errorf("failed to set GOOGLE_CREDENTIALS env var: %w", err)
	}

	pm, err := NewPulumiManager("osmedeus-cloud", infraID, statePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create Pulumi manager: %w", err)
	}

	// Set GCP provider configuration
	if err := pm.SetConfig(ctx, "gcp:project", p.projectID, false); err != nil {
		return nil, fmt.Errorf("failed to set GCP project: %w", err)
	}
	if err := pm.SetConfig(ctx, "gcp:region", p.region, false); err != nil {
		return nil, fmt.Errorf("failed to set GCP region: %w", err)
	}
	if err := pm.SetConfig(ctx, "gcp:zone", p.zone, false); err != nil {
		return nil, fmt.Errorf("failed to set GCP zone: %w", err)
	}

	// Run Pulumi program
	if err := pm.Up(ctx, p.createInstanceProgram(infraID, opts)); err != nil {
		return nil, fmt.Errorf("failed to provision GCP instances: %w", err)
	}

	// Extract outputs
	outputs, err := pm.GetOutputs(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get outputs: %w", err)
	}

	infra := &Infrastructure{
		ID:            infraID,
		Provider:      ProviderGCP,
		Mode:          opts.Mode,
		CreatedAt:     time.Now(),
		PulumiStackID: pm.GetStackName(),
		StatePath:     statePath,
		Resources:     buildResourcesFromOutputs(infraID, opts.InstanceCount, outputs),
		Metadata: map[string]interface{}{
			"project":  p.projectID,
			"region":   p.region,
			"zone":     p.zone,
			"ssh_user": "root",
		},
	}

	return infra, nil
}

// DestroyInfrastructure tears down GCP resources
func (p *GCPProvider) DestroyInfrastructure(ctx context.Context, infra *Infrastructure) error {
	// Set GOOGLE_CREDENTIALS env var from credentials file content
	credContent, err := os.ReadFile(p.credentialsFile)
	if err != nil {
		return fmt.Errorf("failed to read GCP credentials file: %w", err)
	}
	if err := os.Setenv("GOOGLE_CREDENTIALS", string(credContent)); err != nil {
		return fmt.Errorf("failed to set GOOGLE_CREDENTIALS env var: %w", err)
	}

	statePath := infra.StatePath

	pm, err := NewPulumiManager("osmedeus-cloud", infra.PulumiStackID, statePath)
	if err != nil {
		return fmt.Errorf("failed to create Pulumi manager: %w", err)
	}

	if err := pm.SetConfig(ctx, "gcp:project", p.projectID, false); err != nil {
		return fmt.Errorf("failed to set GCP project: %w", err)
	}
	if err := pm.SetConfig(ctx, "gcp:region", p.region, false); err != nil {
		return fmt.Errorf("failed to set GCP region: %w", err)
	}
	if err := pm.SetConfig(ctx, "gcp:zone", p.zone, false); err != nil {
		return fmt.Errorf("failed to set GCP zone: %w", err)
	}

	return pm.Destroy(ctx)
}

// GetStatus retrieves the current status of infrastructure
func (p *GCPProvider) GetStatus(ctx context.Context, infra *Infrastructure) (*InfraStatus, error) {
	status := &InfraStatus{
		Status:     "running",
		TotalCount: len(infra.Resources),
		Details:    make([]ResourceStatus, 0, len(infra.Resources)),
	}

	for _, res := range infra.Resources {
		rs := ResourceStatus{
			ResourceID:       res.ID,
			Status:           res.Status,
			WorkerRegistered: res.WorkerID != "",
		}

		if res.WorkerID != "" {
			status.WorkersRegistered++
		}

		if res.Status == "running" || res.Status == "RUNNING" {
			status.ReadyCount++
		}

		status.Details = append(status.Details, rs)
	}

	return status, nil
}

// Type returns the provider type
func (p *GCPProvider) Type() ProviderType {
	return ProviderGCP
}

// createInstanceProgram creates a Pulumi program for GCP compute instances
func (p *GCPProvider) createInstanceProgram(infraID string, opts *CreateOptions) pulumi.RunFunc {
	suffix := infraSuffix(infraID)

	return func(ctx *pulumi.Context) error {
		userData := GenerateCloudInit(opts.RedisURL, opts.SSHPublicKey, opts.SetupCommands)

		// Create firewall rule allowing SSH access
		_, err := compute.NewFirewall(ctx, "osmedeus-allow-ssh", &compute.FirewallArgs{
			Network: pulumi.String("default"),
			Allows: compute.FirewallAllowArray{
				&compute.FirewallAllowArgs{
					Protocol: pulumi.String("tcp"),
					Ports: pulumi.StringArray{
						pulumi.String("22"),
					},
				},
			},
			SourceRanges: pulumi.StringArray{
				pulumi.String("0.0.0.0/0"),
			},
			TargetTags: pulumi.StringArray{
				pulumi.String("osmedeus-worker"),
			},
		})
		if err != nil {
			return fmt.Errorf("failed to create firewall rule: %w", err)
		}

		// Determine boot disk image
		image := fmt.Sprintf("ubuntu-os-cloud/%s", p.imageFamily)
		if opts.ImageID != "" {
			image = opts.ImageID
		}

		// Create compute instances
		for i := 0; i < opts.InstanceCount; i++ {
			instanceName := fmt.Sprintf("osmw-%s-%d", suffix, i)

			instanceArgs := &compute.InstanceArgs{
				MachineType: pulumi.String(p.machineType),
				Zone:        pulumi.String(p.zone),
				BootDisk: &compute.InstanceBootDiskArgs{
					InitializeParams: &compute.InstanceBootDiskInitializeParamsArgs{
						Image: pulumi.String(image),
					},
				},
				NetworkInterfaces: compute.InstanceNetworkInterfaceArray{
					&compute.InstanceNetworkInterfaceArgs{
						Network: pulumi.String("default"),
						AccessConfigs: compute.InstanceNetworkInterfaceAccessConfigArray{
							&compute.InstanceNetworkInterfaceAccessConfigArgs{},
						},
					},
				},
				Metadata: pulumi.StringMap{
					"startup-script": pulumi.String(userData),
					"ssh-keys":       pulumi.Sprintf("root:%s", opts.SSHPublicKey),
				},
				Tags: pulumi.StringArray{
					pulumi.String("osmedeus-worker"),
				},
			}

			// Enable preemptible scheduling if requested
			if p.usePreemptible {
				instanceArgs.Scheduling = &compute.InstanceSchedulingArgs{
					Preemptible:    pulumi.Bool(true),
					AutomaticRestart: pulumi.Bool(false),
				}
			}

			instance, err := compute.NewInstance(ctx, instanceName, instanceArgs)
			if err != nil {
				return fmt.Errorf("failed to create instance %s: %w", instanceName, err)
			}

			// Export instance ID
			ctx.Export(fmt.Sprintf("worker-%d-id", i), instance.InstanceId)

			// Export public IP from the first network interface's first access config
			publicIP := instance.NetworkInterfaces.Index(pulumi.Int(0)).AccessConfigs().Index(pulumi.Int(0)).NatIp()
			ctx.Export(fmt.Sprintf("worker-%d-ip", i), publicIP)
		}

		return nil
	}
}

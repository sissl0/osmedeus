package cloud

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	pulumiHcloud "github.com/pulumi/pulumi-hcloud/sdk/go/hcloud"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// HetznerProvider implements the Provider interface for Hetzner Cloud
type HetznerProvider struct {
	token      string
	location   string
	serverType string
	image      string
	sshKeyName string
	client     *hcloud.Client
}

// NewHetznerProvider creates a new Hetzner Cloud provider
func NewHetznerProvider(token, location, serverType, image, sshKeyName string) (*HetznerProvider, error) {
	if token == "" {
		return nil, fmt.Errorf("hetzner cloud token is required")
	}

	// Apply defaults
	if location == "" {
		location = "hel1"
	}
	if serverType == "" {
		serverType = "cx23"
	}
	if image == "" {
		image = "ubuntu-22.04"
	}

	client := hcloud.NewClient(hcloud.WithToken(token))

	return &HetznerProvider{
		token:      token,
		location:   location,
		serverType: serverType,
		image:      image,
		sshKeyName: sshKeyName,
		client:     client,
	}, nil
}

// Validate checks if the provider configuration is valid
func (p *HetznerProvider) Validate(ctx context.Context) error {
	_, err := p.client.ServerType.All(ctx)
	if err != nil {
		return fmt.Errorf("failed to validate Hetzner Cloud credentials: %w", err)
	}
	return nil
}

// EstimateCost estimates the cost for the given configuration
func (p *HetznerProvider) EstimateCost(mode ExecutionMode, instanceCount int) (*CostEstimate, error) {
	if mode != ModeVM {
		return nil, fmt.Errorf("only VM mode is supported for Hetzner Cloud")
	}

	// Approximate USD/hr pricing (Hetzner bills in EUR; these are approximate USD conversions)
	pricing := map[string]float64{
		// Current generation (cost-optimized)
		"cx23":  0.0050,
		"cx33":  0.0094,
		"cx43":  0.0169,
		"cx53":  0.0319,
		"cax11": 0.0044,
		"cax21": 0.0069,
		"cax31": 0.0119,
		"cax41": 0.0219,
		// Previous generation (may still work)
		"cx11":  0.0050,
		"cx21":  0.0094,
		"cx22":  0.0094,
		"cx31":  0.0169,
		"cx32":  0.0169,
		"cx41":  0.0319,
		"cx42":  0.0319,
		"cx51":  0.0609,
		"cx52":  0.0609,
		"cpx11": 0.0064,
		"cpx21": 0.0109,
		"cpx31": 0.0199,
		"cpx41": 0.0359,
		"cpx51": 0.0679,
	}

	hourlyRate, ok := pricing[p.serverType]
	if !ok {
		// Default to cx23 pricing if unknown
		hourlyRate = 0.0050
	}

	totalHourlyRate := hourlyRate * float64(instanceCount)

	return &CostEstimate{
		HourlyCost: totalHourlyRate,
		DailyCost:  totalHourlyRate * 24,
		Currency:   "USD",
		Breakdown: map[string]float64{
			"compute": totalHourlyRate,
		},
		Notes: []string{
			fmt.Sprintf("%d x %s servers @ $%.4f/hr each", instanceCount, p.serverType, hourlyRate),
			"Hetzner bills in EUR; prices shown are approximate USD conversions",
		},
	}, nil
}

// CreateInfrastructure provisions Hetzner Cloud servers
func (p *HetznerProvider) CreateInfrastructure(ctx context.Context, opts *CreateOptions) (*Infrastructure, error) {
	infraID := fmt.Sprintf("cloud-hetzner-%d", time.Now().Unix())
	statePath := opts.StatePath

	pm, err := NewPulumiManager("osmedeus-cloud", infraID, statePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create Pulumi manager: %w", err)
	}

	// Set provider credentials
	if err := pm.SetConfig(ctx, "hcloud:token", p.token, true); err != nil {
		return nil, fmt.Errorf("failed to set Hetzner Cloud token: %w", err)
	}

	// Run Pulumi program
	if err := pm.Up(ctx, p.createServerProgram(ctx, infraID, opts)); err != nil {
		return nil, fmt.Errorf("failed to provision servers: %w", err)
	}

	// Extract outputs
	outputs, err := pm.GetOutputs(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get outputs: %w", err)
	}

	infra := &Infrastructure{
		ID:            infraID,
		Provider:      ProviderHetzner,
		Mode:          opts.Mode,
		CreatedAt:     time.Now(),
		PulumiStackID: pm.GetStackName(),
		StatePath:     statePath,
		Resources:     buildResourcesFromOutputs(infraID, opts.InstanceCount, outputs),
		Metadata: map[string]interface{}{
			"location":    p.location,
			"server_type": p.serverType,
			"ssh_user":    "root",
		},
	}

	return infra, nil
}

// DestroyInfrastructure tears down Hetzner Cloud resources
func (p *HetznerProvider) DestroyInfrastructure(ctx context.Context, infra *Infrastructure) error {
	statePath := infra.StatePath

	pm, err := NewPulumiManager("osmedeus-cloud", infra.PulumiStackID, statePath)
	if err != nil {
		return fmt.Errorf("failed to create Pulumi manager: %w", err)
	}

	if err := pm.SetConfig(ctx, "hcloud:token", p.token, true); err != nil {
		return fmt.Errorf("failed to set Hetzner Cloud token: %w", err)
	}

	return pm.Destroy(ctx)
}

// GetStatus retrieves the current status of infrastructure
func (p *HetznerProvider) GetStatus(ctx context.Context, infra *Infrastructure) (*InfraStatus, error) {
	status := &InfraStatus{
		Status:     "running",
		TotalCount: len(infra.Resources),
		Details:    make([]ResourceStatus, 0, len(infra.Resources)),
	}

	for _, res := range infra.Resources {
		rs := ResourceStatus{
			ResourceID:       res.ID,
			WorkerRegistered: res.WorkerID != "",
		}

		if res.WorkerID != "" {
			status.WorkersRegistered++
		}

		// Query server status via API
		serverID, err := strconv.ParseInt(res.ID, 10, 64)
		if err == nil {
			server, _, apiErr := p.client.Server.GetByID(ctx, serverID)
			if apiErr == nil && server != nil {
				rs.Status = string(server.Status)
				if server.Status == hcloud.ServerStatusRunning {
					status.ReadyCount++
				}
			} else if apiErr != nil {
				rs.Status = "unknown"
				rs.Message = apiErr.Error()
			} else {
				rs.Status = "not_found"
				rs.Message = "server not found"
			}
		} else {
			rs.Status = "unknown"
			rs.Message = "invalid server ID"
		}

		status.Details = append(status.Details, rs)
	}

	return status, nil
}

// Type returns the provider type
func (p *HetznerProvider) Type() ProviderType {
	return ProviderHetzner
}

func (p *HetznerProvider) findExistingSSHKey(ctx context.Context, publicKey string) (string, bool) {
	fp, err := sshPublicKeyFingerprint(publicKey)
	if err != nil {
		return "", false
	}
	key, _, err := p.client.SSHKey.GetByFingerprint(ctx, fp)
	if err != nil || key == nil {
		return "", false
	}
	return key.Name, true
}

// createServerProgram creates a Pulumi program for Hetzner Cloud servers
func (p *HetznerProvider) createServerProgram(parentCtx context.Context, infraID string, opts *CreateOptions) pulumi.RunFunc {
	suffix := infraSuffix(infraID)
	existingKeyName, keyExists := p.findExistingSSHKey(parentCtx, opts.SSHPublicKey)

	return func(ctx *pulumi.Context) error {

		userData := GenerateCloudInit(opts.RedisURL, opts.SSHPublicKey, opts.SetupCommands)

		// Use existing SSH key if found, otherwise create a new one
		var sshKeyName pulumi.StringInput
		if keyExists {
			sshKeyName = pulumi.String(existingKeyName)
		} else {
			sshKey, err := pulumiHcloud.NewSshKey(ctx, "osmedeus-ssh-key", &pulumiHcloud.SshKeyArgs{
				Name:      pulumi.String(fmt.Sprintf("osmedeus-key-%s", suffix)),
				PublicKey: pulumi.String(opts.SSHPublicKey),
			})
			if err != nil {
				return fmt.Errorf("failed to create SSH key: %w", err)
			}
			sshKeyName = sshKey.Name
		}

		// Create firewall allowing SSH
		fw, err := pulumiHcloud.NewFirewall(ctx, "osmedeus-firewall", &pulumiHcloud.FirewallArgs{
			Name: pulumi.String(fmt.Sprintf("osmedeus-fw-%s", suffix)),
			Rules: pulumiHcloud.FirewallRuleArray{
				&pulumiHcloud.FirewallRuleArgs{
					Direction: pulumi.String("in"),
					Protocol:  pulumi.String("tcp"),
					Port:      pulumi.String("22"),
					SourceIps: pulumi.StringArray{
						pulumi.String("0.0.0.0/0"),
						pulumi.String("::/0"),
					},
				},
			},
		})
		if err != nil {
			return fmt.Errorf("failed to create firewall: %w", err)
		}

		// Determine image
		image := p.image
		if opts.ImageID != "" {
			image = opts.ImageID
		}

		// Create servers
		for i := 0; i < opts.InstanceCount; i++ {
			serverName := fmt.Sprintf("osmw-%s-%d", suffix, i)

			server, err := pulumiHcloud.NewServer(ctx, serverName, &pulumiHcloud.ServerArgs{
				Name:       pulumi.String(serverName),
				ServerType: pulumi.String(p.serverType),
				Image:      pulumi.String(image),
				Location:   pulumi.String(p.location),
				SshKeys: pulumi.StringArray{
					sshKeyName,
				},
				UserData: pulumi.String(userData),
				FirewallIds: pulumi.IntArray{
					fw.ID().ToStringOutput().ApplyT(func(s string) (int, error) {
						return strconv.Atoi(s)
					}).(pulumi.IntOutput),
				},
				Labels: pulumi.StringMap{
					"osmedeus": pulumi.String("true"),
				},
			})
			if err != nil {
				return fmt.Errorf("failed to create server %s: %w", serverName, err)
			}

			ctx.Export(fmt.Sprintf("worker-%d-ip", i), server.Ipv4Address)
			ctx.Export(fmt.Sprintf("worker-%d-id", i), server.ID())
		}

		return nil
	}
}

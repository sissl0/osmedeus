package cloud

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/digitalocean/godo"
	"github.com/pulumi/pulumi-digitalocean/sdk/v4/go/digitalocean"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"golang.org/x/oauth2"
)

// DigitalOceanProvider implements the Provider interface for DigitalOcean
type DigitalOceanProvider struct {
	token             string
	region            string
	size              string
	snapshotID        string
	sshKeyFingerprint string
	client            *godo.Client
}

// NewDigitalOceanProvider creates a new DigitalOcean provider
func NewDigitalOceanProvider(token, region, size, snapshotID, sshKeyFingerprint string) (*DigitalOceanProvider, error) {
	if token == "" {
		return nil, fmt.Errorf("DigitalOcean token is required")
	}

	// Create DigitalOcean client
	tokenSource := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	oauthClient := oauth2.NewClient(context.Background(), tokenSource)
	client := godo.NewClient(oauthClient)

	return &DigitalOceanProvider{
		token:             token,
		region:            region,
		size:              size,
		snapshotID:        snapshotID,
		sshKeyFingerprint: sshKeyFingerprint,
		client:            client,
	}, nil
}

// Validate checks if the provider configuration is valid
func (p *DigitalOceanProvider) Validate(ctx context.Context) error {
	// Test API access by getting account info
	_, _, err := p.client.Account.Get(ctx)
	if err != nil {
		return fmt.Errorf("failed to validate DigitalOcean credentials: %w", err)
	}
	return nil
}

// EstimateCost estimates the cost for the given configuration
func (p *DigitalOceanProvider) EstimateCost(mode ExecutionMode, instanceCount int) (*CostEstimate, error) {
	if mode != ModeVM {
		return nil, fmt.Errorf("only VM mode is supported for DigitalOcean")
	}

	// Default pricing for common sizes (USD per hour)
	pricing := map[string]float64{
		"s-1vcpu-1gb":   0.00744, // $5/month
		"s-1vcpu-2gb":   0.01116, // $7.5/month
		"s-2vcpu-2gb":   0.01488, // $10/month
		"s-2vcpu-4gb":   0.02232, // $15/month
		"s-4vcpu-8gb":   0.04464, // $30/month
		"s-8vcpu-16gb":  0.08928, // $60/month
		"s-16vcpu-32gb": 0.17856, // $120/month
	}

	hourlyRate, ok := pricing[p.size]
	if !ok {
		// Default to s-2vcpu-4gb pricing if unknown
		hourlyRate = 0.02232
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
			fmt.Sprintf("%d x %s droplets @ $%.4f/hr each", instanceCount, p.size, hourlyRate),
		},
	}, nil
}

// CreateInfrastructure provisions DigitalOcean droplets
func (p *DigitalOceanProvider) CreateInfrastructure(ctx context.Context, opts *CreateOptions) (*Infrastructure, error) {
	infraID := fmt.Sprintf("cloud-do-%d", time.Now().Unix())
	statePath := opts.StatePath

	pm, err := NewPulumiManager("osmedeus-cloud", infraID, statePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create Pulumi manager: %w", err)
	}

	// Set provider credentials
	if err := pm.SetConfig(ctx, "digitalocean:token", p.token, true); err != nil {
		return nil, fmt.Errorf("failed to set DigitalOcean token: %w", err)
	}

	// Run Pulumi program
	if err := pm.Up(ctx, p.createDropletProgram(ctx, infraID, opts)); err != nil {
		return nil, fmt.Errorf("failed to provision droplets: %w", err)
	}

	// Extract outputs
	outputs, err := pm.GetOutputs(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get outputs: %w", err)
	}

	infra := &Infrastructure{
		ID:            infraID,
		Provider:      ProviderDigitalOcean,
		Mode:          opts.Mode,
		CreatedAt:     time.Now(),
		PulumiStackID: pm.GetStackName(),
		StatePath:     statePath,
		Resources:     buildResourcesFromOutputs(infraID, opts.InstanceCount, outputs),
		Metadata: map[string]interface{}{
			"region":   p.region,
			"size":     p.size,
			"ssh_user": "root",
		},
	}

	return infra, nil
}

// DestroyInfrastructure tears down DigitalOcean resources
func (p *DigitalOceanProvider) DestroyInfrastructure(ctx context.Context, infra *Infrastructure) error {
	statePath := infra.StatePath

	pm, err := NewPulumiManager("osmedeus-cloud", infra.PulumiStackID, statePath)
	if err != nil {
		return fmt.Errorf("failed to create Pulumi manager: %w", err)
	}

	if err := pm.SetConfig(ctx, "digitalocean:token", p.token, true); err != nil {
		return fmt.Errorf("failed to set DigitalOcean token: %w", err)
	}

	return pm.Destroy(ctx)
}

// GetStatus retrieves the current status of infrastructure
func (p *DigitalOceanProvider) GetStatus(ctx context.Context, infra *Infrastructure) (*InfraStatus, error) {
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

		// Query droplet status via API
		dropletID, err := strconv.Atoi(res.ID)
		if err == nil {
			droplet, _, apiErr := p.client.Droplets.Get(ctx, dropletID)
			if apiErr == nil {
				rs.Status = droplet.Status
				if droplet.Status == "active" {
					status.ReadyCount++
				}
			} else {
				rs.Status = "unknown"
				rs.Message = apiErr.Error()
			}
		} else {
			rs.Status = "unknown"
			rs.Message = "invalid droplet ID"
		}

		status.Details = append(status.Details, rs)
	}

	return status, nil
}

// Type returns the provider type
func (p *DigitalOceanProvider) Type() ProviderType {
	return ProviderDigitalOcean
}

// findExistingSSHKey checks if an SSH key with the same fingerprint already exists in DigitalOcean
func (p *DigitalOceanProvider) findExistingSSHKey(ctx context.Context, publicKey string) (string, bool) {
	fp, err := sshPublicKeyFingerprint(publicKey)
	if err != nil {
		return "", false
	}
	key, _, err := p.client.Keys.GetByFingerprint(ctx, fp)
	if err != nil || key == nil {
		return "", false
	}
	return key.Fingerprint, true
}

// createDropletProgram creates a Pulumi program for DigitalOcean droplets
func (p *DigitalOceanProvider) createDropletProgram(parentCtx context.Context, infraID string, opts *CreateOptions) pulumi.RunFunc {
	suffix := infraSuffix(infraID)
	existingFingerprint, keyExists := p.findExistingSSHKey(parentCtx, opts.SSHPublicKey)

	return func(ctx *pulumi.Context) error {

		userData := GenerateCloudInit(opts.RedisURL, opts.SSHPublicKey, opts.SetupCommands)

		// Use existing SSH key if found, otherwise create a new one
		var sshKeyFingerprint pulumi.StringInput
		if keyExists {
			sshKeyFingerprint = pulumi.String(existingFingerprint)
		} else {
			sshKey, err := digitalocean.NewSshKey(ctx, "osmedeus-ssh-key", &digitalocean.SshKeyArgs{
				Name:      pulumi.String(fmt.Sprintf("osmedeus-key-%s", suffix)),
				PublicKey: pulumi.String(opts.SSHPublicKey),
			})
			if err != nil {
				return fmt.Errorf("failed to create SSH key: %w", err)
			}
			sshKeyFingerprint = sshKey.Fingerprint
		}

		// Create firewall allowing SSH
		fw, err := digitalocean.NewFirewall(ctx, "osmedeus-firewall", &digitalocean.FirewallArgs{
			Name: pulumi.String(fmt.Sprintf("osmedeus-fw-%s", suffix)),
			InboundRules: digitalocean.FirewallInboundRuleArray{
				&digitalocean.FirewallInboundRuleArgs{
					Protocol:  pulumi.String("tcp"),
					PortRange: pulumi.String("22"),
					SourceAddresses: pulumi.StringArray{
						pulumi.String("0.0.0.0/0"),
						pulumi.String("::/0"),
					},
				},
			},
			OutboundRules: digitalocean.FirewallOutboundRuleArray{
				&digitalocean.FirewallOutboundRuleArgs{
					Protocol:  pulumi.String("tcp"),
					PortRange: pulumi.String("1-65535"),
					DestinationAddresses: pulumi.StringArray{
						pulumi.String("0.0.0.0/0"),
						pulumi.String("::/0"),
					},
				},
				&digitalocean.FirewallOutboundRuleArgs{
					Protocol:  pulumi.String("udp"),
					PortRange: pulumi.String("1-65535"),
					DestinationAddresses: pulumi.StringArray{
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
		image := p.snapshotID
		if image == "" {
			image = "ubuntu-22-04-x64"
		}
		if opts.ImageID != "" {
			image = opts.ImageID
		}

		// Create droplets
		for i := 0; i < opts.InstanceCount; i++ {
			dropletName := fmt.Sprintf("osmw-%s-%d", suffix, i)

			droplet, err := digitalocean.NewDroplet(ctx, dropletName, &digitalocean.DropletArgs{
				Image:    pulumi.String(image),
				Region:   digitalocean.Region(p.region),
				Size:     digitalocean.DropletSlug(p.size),
				UserData: pulumi.String(userData),
				SshKeys: pulumi.StringArray{
					sshKeyFingerprint,
				},
				Tags: pulumi.StringArray{
					pulumi.String("osmedeus"),
					pulumi.String("worker"),
				},
			})
			if err != nil {
				return fmt.Errorf("failed to create droplet %s: %w", dropletName, err)
			}

			ctx.Export(fmt.Sprintf("worker-%d-ip", i), droplet.Ipv4Address)
			ctx.Export(fmt.Sprintf("worker-%d-id", i), droplet.ID())
		}

		_ = fw

		return nil
	}
}

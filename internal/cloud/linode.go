package cloud

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/linode/linodego"
	"github.com/pulumi/pulumi-linode/sdk/v4/go/linode"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"golang.org/x/oauth2"
)

// LinodeProvider implements the Provider interface for Linode
type LinodeProvider struct {
	token      string
	region     string
	linodeType string
	image      string
	client     linodego.Client
}

// NewLinodeProvider creates a new Linode provider
func NewLinodeProvider(token, region, linodeType, image string) (*LinodeProvider, error) {
	if token == "" {
		return nil, fmt.Errorf("linode token is required")
	}

	if region == "" {
		region = "us-east"
	}
	if linodeType == "" {
		linodeType = "g6-standard-2"
	}
	if image == "" {
		image = "linode/ubuntu22.04"
	}

	// Create linodego client
	tokenSource := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	oauthClient := &http.Client{Transport: &oauth2.Transport{Source: tokenSource}}
	client := linodego.NewClient(oauthClient)

	return &LinodeProvider{
		token:      token,
		region:     region,
		linodeType: linodeType,
		image:      image,
		client:     client,
	}, nil
}

// Validate checks if the provider configuration is valid
func (p *LinodeProvider) Validate(ctx context.Context) error {
	_, err := p.client.GetAccount(ctx)
	if err != nil {
		return fmt.Errorf("failed to validate Linode credentials: %w", err)
	}
	return nil
}

// EstimateCost estimates the cost for the given configuration
func (p *LinodeProvider) EstimateCost(mode ExecutionMode, instanceCount int) (*CostEstimate, error) {
	if mode != ModeVM {
		return nil, fmt.Errorf("only VM mode is supported for Linode")
	}

	// Default pricing for common types (USD per hour)
	pricing := map[string]float64{
		"g6-nanode-1":    0.0075,
		"g6-standard-1":  0.0075,
		"g6-standard-2":  0.0150,
		"g6-standard-4":  0.0300,
		"g6-standard-6":  0.0600,
		"g6-standard-8":  0.0900,
		"g6-dedicated-2": 0.0450,
		"g6-dedicated-4": 0.0900,
	}

	hourlyRate, ok := pricing[p.linodeType]
	if !ok {
		// Default to g6-standard-2 pricing if unknown
		hourlyRate = 0.0150
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
			fmt.Sprintf("%d x %s instances @ $%.4f/hr each", instanceCount, p.linodeType, hourlyRate),
		},
	}, nil
}

// CreateInfrastructure provisions Linode instances
func (p *LinodeProvider) CreateInfrastructure(ctx context.Context, opts *CreateOptions) (*Infrastructure, error) {
	infraID := fmt.Sprintf("cloud-linode-%d", time.Now().Unix())
	statePath := opts.StatePath

	pm, err := NewPulumiManager("osmedeus-cloud", infraID, statePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create Pulumi manager: %w", err)
	}

	// Set provider credentials
	if err := pm.SetConfig(ctx, "linode:token", p.token, true); err != nil {
		return nil, fmt.Errorf("failed to set Linode token: %w", err)
	}

	// Run Pulumi program
	if err := pm.Up(ctx, p.createInstanceProgram(infraID, opts)); err != nil {
		return nil, fmt.Errorf("failed to provision instances: %w", err)
	}

	// Extract outputs
	outputs, err := pm.GetOutputs(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get outputs: %w", err)
	}

	infra := &Infrastructure{
		ID:            infraID,
		Provider:      ProviderLinode,
		Mode:          opts.Mode,
		CreatedAt:     time.Now(),
		PulumiStackID: pm.GetStackName(),
		StatePath:     statePath,
		Resources:     buildResourcesFromOutputs(infraID, opts.InstanceCount, outputs),
		Metadata: map[string]interface{}{
			"region":   p.region,
			"type":     p.linodeType,
			"ssh_user": "root",
		},
	}

	return infra, nil
}

// DestroyInfrastructure tears down Linode resources
func (p *LinodeProvider) DestroyInfrastructure(ctx context.Context, infra *Infrastructure) error {
	statePath := infra.StatePath

	pm, err := NewPulumiManager("osmedeus-cloud", infra.PulumiStackID, statePath)
	if err != nil {
		return fmt.Errorf("failed to create Pulumi manager: %w", err)
	}

	if err := pm.SetConfig(ctx, "linode:token", p.token, true); err != nil {
		return fmt.Errorf("failed to set Linode token: %w", err)
	}

	return pm.Destroy(ctx)
}

// GetStatus retrieves the current status of infrastructure
func (p *LinodeProvider) GetStatus(ctx context.Context, infra *Infrastructure) (*InfraStatus, error) {
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

		// Query instance status via API
		instanceID, err := strconv.Atoi(res.ID)
		if err == nil {
			instance, apiErr := p.client.GetInstance(ctx, instanceID)
			if apiErr == nil {
				rs.Status = string(instance.Status)
				if instance.Status == linodego.InstanceRunning {
					status.ReadyCount++
				}
			} else {
				rs.Status = "unknown"
				rs.Message = apiErr.Error()
			}
		} else {
			rs.Status = "unknown"
			rs.Message = "invalid instance ID"
		}

		status.Details = append(status.Details, rs)
	}

	return status, nil
}

// Type returns the provider type
func (p *LinodeProvider) Type() ProviderType {
	return ProviderLinode
}

// createInstanceProgram creates a Pulumi program for Linode instances
func (p *LinodeProvider) createInstanceProgram(infraID string, opts *CreateOptions) pulumi.RunFunc {
	suffix := infraSuffix(infraID)

	return func(ctx *pulumi.Context) error {

		userData := GenerateCloudInit(opts.RedisURL, opts.SSHPublicKey, opts.SetupCommands)
		userDataB64 := base64.StdEncoding.EncodeToString([]byte(userData))

		// Upload SSH key
		sshKey, err := linode.NewSshKey(ctx, "osmedeus-ssh-key", &linode.SshKeyArgs{
			Label:  pulumi.String(fmt.Sprintf("osmedeus-key-%s", suffix)),
			SshKey: pulumi.String(opts.SSHPublicKey),
		})
		if err != nil {
			return fmt.Errorf("failed to create SSH key: %w", err)
		}

		// Create firewall allowing SSH inbound and all outbound
		fw, err := linode.NewFirewall(ctx, "osmedeus-firewall", &linode.FirewallArgs{
			Label:          pulumi.String(fmt.Sprintf("osmedeus-fw-%s", suffix)),
			InboundPolicy:  pulumi.String("DROP"),
			OutboundPolicy: pulumi.String("ACCEPT"),
			Inbounds: linode.FirewallInboundArray{
				&linode.FirewallInboundArgs{
					Action:   pulumi.String("ACCEPT"),
					Label:    pulumi.String("allow-ssh"),
					Protocol: pulumi.String("TCP"),
					Ports:    pulumi.String("22"),
					Ipv4s:    pulumi.StringArray{pulumi.String("0.0.0.0/0")},
					Ipv6s:    pulumi.StringArray{pulumi.String("::/0")},
				},
			},
			Outbounds: linode.FirewallOutboundArray{
				&linode.FirewallOutboundArgs{
					Action:   pulumi.String("ACCEPT"),
					Label:    pulumi.String("allow-all-tcp"),
					Protocol: pulumi.String("TCP"),
					Ports:    pulumi.String("1-65535"),
					Ipv4s:    pulumi.StringArray{pulumi.String("0.0.0.0/0")},
					Ipv6s:    pulumi.StringArray{pulumi.String("::/0")},
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

		// Create instances
		for i := 0; i < opts.InstanceCount; i++ {
			instanceName := fmt.Sprintf("osmw-%s-%d", suffix, i)

			instance, err := linode.NewInstance(ctx, instanceName, &linode.InstanceArgs{
				Label:          pulumi.String(instanceName),
				Type:           pulumi.String(p.linodeType),
				Region:         pulumi.String(p.region),
				Image:          pulumi.String(image),
				AuthorizedKeys: pulumi.StringArray{pulumi.String(opts.SSHPublicKey)},
				Metadatas: linode.InstanceMetadataArray{
					&linode.InstanceMetadataArgs{
						UserData: pulumi.String(userDataB64),
					},
				},
				Tags: pulumi.StringArray{
					pulumi.String("osmedeus"),
					pulumi.String("worker"),
				},
			})
			if err != nil {
				return fmt.Errorf("failed to create instance %s: %w", instanceName, err)
			}

			ctx.Export(fmt.Sprintf("worker-%d-ip", i), instance.IpAddress) //nolint:staticcheck // IpAddress is deprecated but no replacement in SDK v4
			ctx.Export(fmt.Sprintf("worker-%d-id", i), instance.ID())
		}

		// Reference sshKey and fw to avoid unused variable errors
		_ = sshKey
		_ = fw

		return nil
	}
}

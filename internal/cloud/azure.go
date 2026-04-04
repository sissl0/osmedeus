package cloud

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pulumi/pulumi-azure-native-sdk/compute/v2"
	"github.com/pulumi/pulumi-azure-native-sdk/network/v2"
	"github.com/pulumi/pulumi-azure-native-sdk/resources/v2"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// AzureProvider implements the Provider interface for Azure
type AzureProvider struct {
	subscriptionID string
	tenantID       string
	clientID       string
	clientSecret   string
	location       string
	vmSize         string
	imageReference string // "Publisher:Offer:SKU:Version"
}

// NewAzureProvider creates a new Azure provider
func NewAzureProvider(subscriptionID, tenantID, clientID, clientSecret, location, vmSize, imageReference string) (*AzureProvider, error) {
	if subscriptionID == "" {
		return nil, fmt.Errorf("azure subscription ID is required")
	}
	if clientID == "" {
		return nil, fmt.Errorf("azure client ID is required")
	}
	if clientSecret == "" {
		return nil, fmt.Errorf("azure client secret is required")
	}

	if location == "" {
		location = "eastus"
	}
	if vmSize == "" {
		vmSize = "Standard_B2s"
	}
	if imageReference == "" {
		imageReference = "Canonical:0001-com-ubuntu-server-jammy:22_04-lts:latest"
	}

	return &AzureProvider{
		subscriptionID: subscriptionID,
		tenantID:       tenantID,
		clientID:       clientID,
		clientSecret:   clientSecret,
		location:       location,
		vmSize:         vmSize,
		imageReference: imageReference,
	}, nil
}

// Validate checks if the provider configuration is valid
func (p *AzureProvider) Validate(ctx context.Context) error {
	if p.subscriptionID == "" {
		return fmt.Errorf("azure subscription ID is required")
	}
	if p.clientID == "" {
		return fmt.Errorf("azure client ID is required")
	}
	if p.clientSecret == "" {
		return fmt.Errorf("azure client secret is required")
	}

	// Set ARM environment variables for Pulumi azure-native provider
	_ = os.Setenv("ARM_SUBSCRIPTION_ID", p.subscriptionID)
	_ = os.Setenv("ARM_TENANT_ID", p.tenantID)
	_ = os.Setenv("ARM_CLIENT_ID", p.clientID)
	_ = os.Setenv("ARM_CLIENT_SECRET", p.clientSecret)

	// Pulumi will validate credentials when running Up
	return nil
}

// EstimateCost estimates the cost for the given configuration
func (p *AzureProvider) EstimateCost(mode ExecutionMode, instanceCount int) (*CostEstimate, error) {
	if mode != ModeVM {
		return nil, fmt.Errorf("only VM mode is supported for Azure")
	}

	// Default pricing for common VM sizes (USD per hour, East US region)
	pricing := map[string]float64{
		"Standard_B1s":    0.0104,
		"Standard_B2s":    0.0416,
		"Standard_B2ms":   0.0832,
		"Standard_D2s_v3": 0.0960,
		"Standard_D4s_v3": 0.1920,
		"Standard_F2s_v2": 0.0850,
		"Standard_F4s_v2": 0.1700,
	}

	hourlyRate, ok := pricing[p.vmSize]
	if !ok {
		// Default to Standard_B2s pricing if unknown
		hourlyRate = 0.0416
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
			fmt.Sprintf("%d x %s VMs @ $%.4f/hr each", instanceCount, p.vmSize, hourlyRate),
		},
	}, nil
}

// CreateInfrastructure provisions Azure VMs
func (p *AzureProvider) CreateInfrastructure(ctx context.Context, opts *CreateOptions) (*Infrastructure, error) {
	infraID := fmt.Sprintf("cloud-azure-%d", time.Now().Unix())
	statePath := opts.StatePath

	// Set ARM environment variables for Pulumi azure-native provider
	_ = os.Setenv("ARM_SUBSCRIPTION_ID", p.subscriptionID)
	_ = os.Setenv("ARM_TENANT_ID", p.tenantID)
	_ = os.Setenv("ARM_CLIENT_ID", p.clientID)
	_ = os.Setenv("ARM_CLIENT_SECRET", p.clientSecret)

	pm, err := NewPulumiManager("osmedeus-cloud", infraID, statePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create Pulumi manager: %w", err)
	}

	// Set provider location
	if err := pm.SetConfig(ctx, "azure-native:location", p.location, false); err != nil {
		return nil, fmt.Errorf("failed to set Azure location: %w", err)
	}

	// Run Pulumi program
	if err := pm.Up(ctx, p.createVMProgram(infraID, opts)); err != nil {
		return nil, fmt.Errorf("failed to provision Azure VMs: %w", err)
	}

	// Extract outputs
	outputs, err := pm.GetOutputs(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get outputs: %w", err)
	}

	infra := &Infrastructure{
		ID:            infraID,
		Provider:      ProviderAzure,
		Mode:          opts.Mode,
		CreatedAt:     time.Now(),
		PulumiStackID: pm.GetStackName(),
		StatePath:     statePath,
		Resources:     buildResourcesFromOutputs(infraID, opts.InstanceCount, outputs),
		Metadata: map[string]interface{}{
			"location": p.location,
			"vm_size":  p.vmSize,
			"ssh_user": "azureuser",
		},
	}

	return infra, nil
}

// DestroyInfrastructure tears down Azure resources
func (p *AzureProvider) DestroyInfrastructure(ctx context.Context, infra *Infrastructure) error {
	statePath := infra.StatePath

	// Set ARM environment variables for Pulumi azure-native provider
	_ = os.Setenv("ARM_SUBSCRIPTION_ID", p.subscriptionID)
	_ = os.Setenv("ARM_TENANT_ID", p.tenantID)
	_ = os.Setenv("ARM_CLIENT_ID", p.clientID)
	_ = os.Setenv("ARM_CLIENT_SECRET", p.clientSecret)

	pm, err := NewPulumiManager("osmedeus-cloud", infra.PulumiStackID, statePath)
	if err != nil {
		return fmt.Errorf("failed to create Pulumi manager: %w", err)
	}

	if err := pm.SetConfig(ctx, "azure-native:location", p.location, false); err != nil {
		return fmt.Errorf("failed to set Azure location: %w", err)
	}

	return pm.Destroy(ctx)
}

// GetStatus retrieves the current status of infrastructure
func (p *AzureProvider) GetStatus(ctx context.Context, infra *Infrastructure) (*InfraStatus, error) {
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

		// Use stored status from infrastructure resources
		if res.Status != "" {
			rs.Status = res.Status
		} else {
			rs.Status = "unknown"
		}

		if rs.Status == "running" || rs.Status == "active" {
			status.ReadyCount++
		}

		status.Details = append(status.Details, rs)
	}

	return status, nil
}

// Type returns the provider type
func (p *AzureProvider) Type() ProviderType {
	return ProviderAzure
}

// parseImageReference splits an image reference string "Publisher:Offer:SKU:Version" into its components
func parseImageReference(ref string) (publisher, offer, sku, version string) {
	parts := strings.SplitN(ref, ":", 4)
	if len(parts) != 4 {
		// Return defaults for Ubuntu 22.04 LTS
		return "Canonical", "0001-com-ubuntu-server-jammy", "22_04-lts", "latest"
	}
	return parts[0], parts[1], parts[2], parts[3]
}

// createVMProgram creates a Pulumi program for Azure VMs
func (p *AzureProvider) createVMProgram(infraID string, opts *CreateOptions) pulumi.RunFunc {
	suffix := infraSuffix(infraID)

	return func(ctx *pulumi.Context) error {
		userData := GenerateCloudInit(opts.RedisURL, opts.SSHPublicKey, opts.SetupCommands)
		customData := base64.StdEncoding.EncodeToString([]byte(userData))

		publisher, offer, sku, version := parseImageReference(p.imageReference)

		// Create resource group
		rg, err := resources.NewResourceGroup(ctx, "osmedeus-rg", &resources.ResourceGroupArgs{
			ResourceGroupName: pulumi.Sprintf("osmedeus-rg-%d", time.Now().Unix()),
			Location:          pulumi.String(p.location),
		})
		if err != nil {
			return fmt.Errorf("failed to create resource group: %w", err)
		}

		// Create virtual network
		vnet, err := network.NewVirtualNetwork(ctx, "osmedeus-vnet", &network.VirtualNetworkArgs{
			ResourceGroupName:  rg.Name,
			VirtualNetworkName: pulumi.String("osmedeus-vnet"),
			Location:           pulumi.String(p.location),
			AddressSpace: &network.AddressSpaceArgs{
				AddressPrefixes: pulumi.StringArray{
					pulumi.String("10.0.0.0/16"),
				},
			},
		})
		if err != nil {
			return fmt.Errorf("failed to create virtual network: %w", err)
		}

		// Create subnet
		subnet, err := network.NewSubnet(ctx, "osmedeus-subnet", &network.SubnetArgs{
			ResourceGroupName:  rg.Name,
			VirtualNetworkName: vnet.Name,
			SubnetName:         pulumi.String("osmedeus-subnet"),
			AddressPrefix:      pulumi.String("10.0.1.0/24"),
		})
		if err != nil {
			return fmt.Errorf("failed to create subnet: %w", err)
		}

		// Create network security group allowing SSH inbound
		nsg, err := network.NewNetworkSecurityGroup(ctx, "osmedeus-nsg", &network.NetworkSecurityGroupArgs{
			ResourceGroupName:        rg.Name,
			NetworkSecurityGroupName: pulumi.String("osmedeus-nsg"),
			Location:                 pulumi.String(p.location),
			SecurityRules: network.SecurityRuleTypeArray{
				&network.SecurityRuleTypeArgs{
					Name:                     pulumi.String("allow-ssh"),
					Priority:                 pulumi.Int(1000),
					Direction:                pulumi.String("Inbound"),
					Access:                   pulumi.String("Allow"),
					Protocol:                 pulumi.String("Tcp"),
					SourcePortRange:          pulumi.String("*"),
					DestinationPortRange:     pulumi.String("22"),
					SourceAddressPrefix:      pulumi.String("*"),
					DestinationAddressPrefix: pulumi.String("*"),
				},
			},
		})
		if err != nil {
			return fmt.Errorf("failed to create network security group: %w", err)
		}

		// Create VMs
		for i := 0; i < opts.InstanceCount; i++ {
			workerName := fmt.Sprintf("osmw-%s-%d", suffix, i)

			// Create public IP address
			publicIP, err := network.NewPublicIPAddress(ctx, fmt.Sprintf("osmedeus-pip-%d", i), &network.PublicIPAddressArgs{
				ResourceGroupName:        rg.Name,
				PublicIpAddressName:      pulumi.Sprintf("osmedeus-pip-%d", i),
				Location:                 pulumi.String(p.location),
				PublicIPAllocationMethod: pulumi.String("Dynamic"),
			})
			if err != nil {
				return fmt.Errorf("failed to create public IP for %s: %w", workerName, err)
			}

			// Create network interface
			nic, err := network.NewNetworkInterface(ctx, fmt.Sprintf("osmedeus-nic-%d", i), &network.NetworkInterfaceArgs{
				ResourceGroupName:    rg.Name,
				NetworkInterfaceName: pulumi.Sprintf("osmedeus-nic-%d", i),
				Location:             pulumi.String(p.location),
				IpConfigurations: network.NetworkInterfaceIPConfigurationArray{
					&network.NetworkInterfaceIPConfigurationArgs{
						Name:                      pulumi.String("ipconfig"),
						PrivateIPAllocationMethod: pulumi.String("Dynamic"),
						Subnet: &network.SubnetTypeArgs{
							Id: subnet.ID(),
						},
						PublicIPAddress: &network.PublicIPAddressTypeArgs{
							Id: publicIP.ID(),
						},
					},
				},
				NetworkSecurityGroup: &network.NetworkSecurityGroupTypeArgs{
					Id: nsg.ID(),
				},
			})
			if err != nil {
				return fmt.Errorf("failed to create network interface for %s: %w", workerName, err)
			}

			// Create virtual machine
			vm, err := compute.NewVirtualMachine(ctx, workerName, &compute.VirtualMachineArgs{
				ResourceGroupName: rg.Name,
				VmName:            pulumi.String(workerName),
				Location:          pulumi.String(p.location),
				HardwareProfile: &compute.HardwareProfileArgs{
					VmSize: pulumi.String(p.vmSize),
				},
				OsProfile: &compute.OSProfileArgs{
					ComputerName:  pulumi.String(workerName),
					AdminUsername: pulumi.String("azureuser"),
					CustomData:    pulumi.String(customData),
					LinuxConfiguration: &compute.LinuxConfigurationArgs{
						DisablePasswordAuthentication: pulumi.Bool(true),
						Ssh: &compute.SshConfigurationArgs{
							PublicKeys: compute.SshPublicKeyTypeArray{
								&compute.SshPublicKeyTypeArgs{
									Path:    pulumi.String("/home/azureuser/.ssh/authorized_keys"),
									KeyData: pulumi.String(opts.SSHPublicKey),
								},
							},
						},
					},
				},
				StorageProfile: &compute.StorageProfileArgs{
					ImageReference: &compute.ImageReferenceArgs{
						Publisher: pulumi.String(publisher),
						Offer:    pulumi.String(offer),
						Sku:      pulumi.String(sku),
						Version:  pulumi.String(version),
					},
					OsDisk: &compute.OSDiskArgs{
						CreateOption: pulumi.String("FromImage"),
						ManagedDisk: &compute.ManagedDiskParametersArgs{
							StorageAccountType: pulumi.String("Standard_LRS"),
						},
					},
				},
				NetworkProfile: &compute.NetworkProfileArgs{
					NetworkInterfaces: compute.NetworkInterfaceReferenceArray{
						&compute.NetworkInterfaceReferenceArgs{
							Id: nic.ID(),
						},
					},
				},
			})
			if err != nil {
				return fmt.Errorf("failed to create virtual machine %s: %w", workerName, err)
			}

			// Export the public IP address and VM ID
			// For Dynamic allocation, the IP is only assigned after the VM is created,
			// so we depend on the VM resource to ensure the IP is available
			ctx.Export(fmt.Sprintf("worker-%d-ip", i), pulumi.All(vm.ID(), publicIP.IpAddress).ApplyT(
				func(args []interface{}) string {
					if ip, ok := args[1].(*string); ok && ip != nil {
						return *ip
					}
					return ""
				},
			).(pulumi.StringOutput))
			ctx.Export(fmt.Sprintf("worker-%d-id", i), vm.ID())
		}

		return nil
	}
}

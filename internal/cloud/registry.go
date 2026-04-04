package cloud

import (
	"fmt"

	"github.com/j3ssie/osmedeus/v5/internal/config"
)

// ProviderRegistry manages provider instances
type ProviderRegistry struct {
	providers map[ProviderType]Provider
}

// NewProviderRegistry creates a new provider registry
func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{
		providers: make(map[ProviderType]Provider),
	}
}

// Register registers a provider
func (r *ProviderRegistry) Register(provider Provider) {
	r.providers[provider.Type()] = provider
}

// Get retrieves a provider by type
func (r *ProviderRegistry) Get(providerType ProviderType) (Provider, error) {
	provider, ok := r.providers[providerType]
	if !ok {
		return nil, fmt.Errorf("provider %s not registered", providerType)
	}
	return provider, nil
}

// List returns all registered provider types
func (r *ProviderRegistry) List() []ProviderType {
	types := make([]ProviderType, 0, len(r.providers))
	for t := range r.providers {
		types = append(types, t)
	}
	return types
}

// CreateProvider creates a provider instance based on cloud configuration
func CreateProvider(cfg *config.CloudConfigs, providerType ProviderType) (Provider, error) {
	switch providerType {
	case ProviderDigitalOcean:
		return NewDigitalOceanProvider(
			cfg.Providers.DigitalOcean.Token,
			cfg.Providers.DigitalOcean.Region,
			cfg.Providers.DigitalOcean.Size,
			cfg.Providers.DigitalOcean.SnapshotID,
			cfg.Providers.DigitalOcean.SSHKeyFingerprint,
		)
	case ProviderAWS:
		return NewAWSProvider(
			cfg.Providers.AWS.AccessKeyID,
			cfg.Providers.AWS.SecretAccessKey,
			cfg.Providers.AWS.Region,
			cfg.Providers.AWS.InstanceType,
			cfg.Providers.AWS.AMI,
			cfg.Providers.AWS.AMIFilter,
			cfg.Providers.AWS.UseSpot,
		)
	case ProviderGCP:
		return NewGCPProvider(
			cfg.Providers.GCP.ProjectID,
			cfg.Providers.GCP.CredentialsFile,
			cfg.Providers.GCP.Region,
			cfg.Providers.GCP.Zone,
			cfg.Providers.GCP.MachineType,
			cfg.Providers.GCP.ImageFamily,
			cfg.Providers.GCP.UsePreemptible,
		)
	case ProviderLinode:
		return NewLinodeProvider(
			cfg.Providers.Linode.Token,
			cfg.Providers.Linode.Region,
			cfg.Providers.Linode.Type,
			cfg.Providers.Linode.Image,
		)
	case ProviderAzure:
		return NewAzureProvider(
			cfg.Providers.Azure.SubscriptionID,
			cfg.Providers.Azure.TenantID,
			cfg.Providers.Azure.ClientID,
			cfg.Providers.Azure.ClientSecret,
			cfg.Providers.Azure.Location,
			cfg.Providers.Azure.VMSize,
			cfg.Providers.Azure.ImageReference,
		)
	case ProviderHetzner:
		return NewHetznerProvider(
			cfg.Providers.Hetzner.Token,
			cfg.Providers.Hetzner.Location,
			cfg.Providers.Hetzner.ServerType,
			cfg.Providers.Hetzner.Image,
			cfg.Providers.Hetzner.SSHKeyName,
		)
	default:
		return nil, fmt.Errorf("unknown provider type: %s", providerType)
	}
}

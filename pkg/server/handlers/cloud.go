package handlers

import (
	"context"
	"fmt"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/j3ssie/osmedeus/v5/internal/cloud"
	"github.com/j3ssie/osmedeus/v5/internal/config"
	"github.com/j3ssie/osmedeus/v5/internal/logger"
	"go.uber.org/zap"
)

// loadCloudConfigFromCfg loads and validates cloud configuration
func loadCloudConfigFromCfg(cfg *config.Config) (*config.CloudConfigs, error) {
	if !cfg.Cloud.Enabled {
		return nil, fmt.Errorf("cloud features are not enabled in configuration")
	}
	if cfg.Cloud.CloudSettings == "" {
		return nil, fmt.Errorf("cloud settings path is not configured")
	}
	cloudCfg, err := cloud.LoadCloudConfig(cfg.Cloud.CloudSettings)
	if err != nil {
		return nil, err
	}
	cloud.ResolveTemplatePaths(cloudCfg, cfg.BaseFolder)
	return cloudCfg, nil
}

// CreateCloudInstancesRequest represents a request to provision cloud infrastructure
type CreateCloudInstancesRequest struct {
	Provider      string            `json:"provider"`                  // required: aws, gcp, digitalocean, linode, azure, hetzner
	InstanceCount int               `json:"instance_count"`            // required: number of instances (>= 1)
	InstanceType  string            `json:"instance_type,omitempty"`   // override default instance size
	Region        string            `json:"region,omitempty"`          // override default region
	UseSpot       bool              `json:"use_spot,omitempty"`        // use spot/preemptible instances
	Tags          map[string]string `json:"tags,omitempty"`            // resource tags
}

// EstimateCloudCostRequest represents a cost estimation request
type EstimateCloudCostRequest struct {
	Provider      string `json:"provider"`                // required
	InstanceCount int    `json:"instance_count"`          // required
	InstanceType  string `json:"instance_type,omitempty"` // override
	UseSpot       bool   `json:"use_spot,omitempty"`
}

// ListCloudProviders returns configured cloud providers with their validation status
// @Summary List cloud providers
// @Description Get a list of all configured cloud providers and whether their credentials are valid
// @Tags Cloud
// @Produce json
// @Success 200 {object} map[string]interface{} "List of providers"
// @Failure 400 {object} map[string]interface{} "Cloud not enabled"
// @Security BearerAuth
// @Router /osm/api/cloud/providers [get]
func ListCloudProviders(cfg *config.Config) fiber.Handler {
	return func(c *fiber.Ctx) error {
		cloudCfg, err := loadCloudConfigFromCfg(cfg)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   true,
				"message": err.Error(),
			})
		}

		allProviders := []cloud.ProviderType{
			cloud.ProviderAWS, cloud.ProviderGCP, cloud.ProviderDigitalOcean,
			cloud.ProviderLinode, cloud.ProviderAzure, cloud.ProviderHetzner,
		}

		providers := make([]fiber.Map, 0, len(allProviders))
		for _, pt := range allProviders {
			entry := fiber.Map{
				"name":       string(pt),
				"is_default": string(pt) == cloudCfg.Defaults.Provider,
			}

			provider, createErr := cloud.CreateProvider(cloudCfg, pt)
			if createErr != nil {
				entry["configured"] = false
				entry["status"] = "not_configured"
				entry["message"] = createErr.Error()
			} else {
				entry["configured"] = true
				if valErr := provider.Validate(c.Context()); valErr != nil {
					entry["status"] = "invalid"
					entry["message"] = valErr.Error()
				} else {
					entry["status"] = "valid"
				}
			}

			providers = append(providers, entry)
		}

		return c.JSON(fiber.Map{
			"providers":        providers,
			"default_provider": cloudCfg.Defaults.Provider,
		})
	}
}

// ValidateCloudProvider tests credentials for a specific cloud provider
// @Summary Validate cloud provider
// @Description Test if the credentials for a specific cloud provider are valid
// @Tags Cloud
// @Produce json
// @Param name path string true "Provider name (aws, gcp, digitalocean, linode, azure, hetzner)"
// @Success 200 {object} map[string]interface{} "Validation result"
// @Failure 400 {object} map[string]interface{} "Invalid request"
// @Security BearerAuth
// @Router /osm/api/cloud/providers/{name}/validate [post]
func ValidateCloudProvider(cfg *config.Config) fiber.Handler {
	return func(c *fiber.Ctx) error {
		providerName := c.Params("name")

		validProviders := map[string]bool{
			"aws": true, "gcp": true, "digitalocean": true,
			"linode": true, "azure": true, "hetzner": true,
		}
		if !validProviders[providerName] {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   true,
				"message": "Invalid provider name. Must be one of: aws, gcp, digitalocean, linode, azure, hetzner",
			})
		}

		cloudCfg, err := loadCloudConfigFromCfg(cfg)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   true,
				"message": err.Error(),
			})
		}

		provider, err := cloud.CreateProvider(cloudCfg, cloud.ProviderType(providerName))
		if err != nil {
			return c.JSON(fiber.Map{
				"provider":  providerName,
				"valid":     false,
				"message":   err.Error(),
			})
		}

		if err := provider.Validate(c.Context()); err != nil {
			return c.JSON(fiber.Map{
				"provider":  providerName,
				"valid":     false,
				"message":   err.Error(),
			})
		}

		return c.JSON(fiber.Map{
			"provider": providerName,
			"valid":    true,
			"message":  "Provider credentials are valid",
		})
	}
}

// CreateCloudInstances provisions new cloud infrastructure
// @Summary Create cloud instances
// @Description Provision new cloud infrastructure. Returns immediately with infra ID; poll status endpoint for progress.
// @Tags Cloud
// @Accept json
// @Produce json
// @Param request body CreateCloudInstancesRequest true "Infrastructure configuration"
// @Success 202 {object} map[string]interface{} "Provisioning started"
// @Failure 400 {object} map[string]interface{} "Invalid request"
// @Security BearerAuth
// @Router /osm/api/cloud/instances [post]
func CreateCloudInstances(cfg *config.Config) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var req CreateCloudInstancesRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   true,
				"message": "Invalid request body",
			})
		}

		if req.Provider == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   true,
				"message": "provider is required",
			})
		}

		if req.InstanceCount < 1 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   true,
				"message": "instance_count must be at least 1",
			})
		}

		cloudCfg, err := loadCloudConfigFromCfg(cfg)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   true,
				"message": err.Error(),
			})
		}

		provider, err := cloud.CreateProvider(cloudCfg, cloud.ProviderType(req.Provider))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   true,
				"message": fmt.Sprintf("Failed to create provider %q: %v", req.Provider, err),
			})
		}

		if err := provider.Validate(c.Context()); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   true,
				"message": fmt.Sprintf("Provider validation failed: %v", err),
			})
		}

		infraID := uuid.New().String()[:8]

		tags := req.Tags
		if tags == nil {
			tags = make(map[string]string)
		}
		tags["source"] = "api"
		tags["infra_id"] = infraID

		createOpts := &cloud.CreateOptions{
			Mode:          cloud.ModeVM,
			InstanceCount: req.InstanceCount,
			InstanceType:  req.InstanceType,
			UseSpot:       req.UseSpot,
			Tags:          tags,
		}

		// Provision in background
		go func() {
			ctx := context.Background()
			lgr := logger.Get()

			lm := cloud.NewLifecycleManager(cloudCfg, provider, nil)
			infra, createErr := lm.CreateAndRun(ctx, createOpts)
			if createErr != nil {
				lgr.Error("Failed to provision cloud infrastructure",
					zap.String("infra_id", infraID),
					zap.Error(createErr))
				return
			}

			lgr.Info("Cloud infrastructure provisioned",
				zap.String("infra_id", infra.ID),
				zap.Int("resources", len(infra.Resources)))
		}()

		return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
			"message":  "Infrastructure provisioning started",
			"infra_id": infraID,
			"status":   "provisioning",
			"poll_url": fmt.Sprintf("/osm/api/cloud/instances/%s/status", infraID),
		})
	}
}

// ListCloudInstances returns all saved infrastructure states
// @Summary List cloud instances
// @Description Get a list of all cloud infrastructure (active and saved)
// @Tags Cloud
// @Produce json
// @Success 200 {object} map[string]interface{} "List of infrastructure"
// @Failure 400 {object} map[string]interface{} "Cloud not enabled"
// @Security BearerAuth
// @Router /osm/api/cloud/instances [get]
func ListCloudInstances(cfg *config.Config) fiber.Handler {
	return func(c *fiber.Ctx) error {
		cloudCfg, err := loadCloudConfigFromCfg(cfg)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   true,
				"message": err.Error(),
			})
		}

		infrastructures, err := cloud.ListInfrastructures(cloudCfg.State.Path)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error":   true,
				"message": fmt.Sprintf("Failed to list infrastructure: %v", err),
			})
		}

		data := make([]fiber.Map, 0, len(infrastructures))
		for _, infra := range infrastructures {
			resources := make([]fiber.Map, 0, len(infra.Resources))
			for _, r := range infra.Resources {
				resources = append(resources, fiber.Map{
					"type":       r.Type,
					"id":         r.ID,
					"name":       r.Name,
					"public_ip":  r.PublicIP,
					"private_ip": r.PrivateIP,
					"status":     r.Status,
					"worker_id":  r.WorkerID,
				})
			}

			data = append(data, fiber.Map{
				"id":         infra.ID,
				"provider":   string(infra.Provider),
				"mode":       string(infra.Mode),
				"created_at": infra.CreatedAt,
				"resources":  resources,
			})
		}

		return c.JSON(fiber.Map{
			"data":  data,
			"count": len(data),
		})
	}
}

// GetCloudInstance returns details for a specific infrastructure
// @Summary Get cloud instance details
// @Description Get details for a specific cloud infrastructure by ID
// @Tags Cloud
// @Produce json
// @Param id path string true "Infrastructure ID"
// @Success 200 {object} map[string]interface{} "Infrastructure details"
// @Failure 404 {object} map[string]interface{} "Infrastructure not found"
// @Security BearerAuth
// @Router /osm/api/cloud/instances/{id} [get]
func GetCloudInstance(cfg *config.Config) fiber.Handler {
	return func(c *fiber.Ctx) error {
		infraID := c.Params("id")

		cloudCfg, err := loadCloudConfigFromCfg(cfg)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   true,
				"message": err.Error(),
			})
		}

		infra, err := cloud.LoadInfrastructureState(infraID, cloudCfg.State.Path)
		if err != nil {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"error":   true,
				"message": fmt.Sprintf("Infrastructure not found: %v", err),
			})
		}

		resources := make([]fiber.Map, 0, len(infra.Resources))
		for _, r := range infra.Resources {
			resources = append(resources, fiber.Map{
				"type":       r.Type,
				"id":         r.ID,
				"name":       r.Name,
				"public_ip":  r.PublicIP,
				"private_ip": r.PrivateIP,
				"status":     r.Status,
				"worker_id":  r.WorkerID,
				"metadata":   r.Metadata,
			})
		}

		return c.JSON(fiber.Map{
			"id":             infra.ID,
			"provider":       string(infra.Provider),
			"mode":           string(infra.Mode),
			"created_at":     infra.CreatedAt,
			"pulumi_stack":   infra.PulumiStackID,
			"resources":      resources,
			"resource_count": len(infra.Resources),
			"metadata":       infra.Metadata,
		})
	}
}

// GetCloudInstanceStatus returns live status for a specific infrastructure
// @Summary Get cloud instance status
// @Description Get live status from the cloud provider for a specific infrastructure
// @Tags Cloud
// @Produce json
// @Param id path string true "Infrastructure ID"
// @Success 200 {object} map[string]interface{} "Infrastructure status"
// @Failure 404 {object} map[string]interface{} "Infrastructure not found"
// @Security BearerAuth
// @Router /osm/api/cloud/instances/{id}/status [get]
func GetCloudInstanceStatus(cfg *config.Config) fiber.Handler {
	return func(c *fiber.Ctx) error {
		infraID := c.Params("id")

		cloudCfg, err := loadCloudConfigFromCfg(cfg)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   true,
				"message": err.Error(),
			})
		}

		infra, err := cloud.LoadInfrastructureState(infraID, cloudCfg.State.Path)
		if err != nil {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"error":   true,
				"message": fmt.Sprintf("Infrastructure not found: %v", err),
			})
		}

		provider, err := cloud.CreateProvider(cloudCfg, infra.Provider)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error":   true,
				"message": fmt.Sprintf("Failed to create provider: %v", err),
			})
		}

		status, err := provider.GetStatus(c.Context(), infra)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error":   true,
				"message": fmt.Sprintf("Failed to get status: %v", err),
			})
		}

		details := make([]fiber.Map, 0, len(status.Details))
		for _, d := range status.Details {
			details = append(details, fiber.Map{
				"resource_id":      d.ResourceID,
				"status":           d.Status,
				"message":          d.Message,
				"worker_registered": d.WorkerRegistered,
			})
		}

		return c.JSON(fiber.Map{
			"infra_id":           infraID,
			"status":             status.Status,
			"ready_count":        status.ReadyCount,
			"total_count":        status.TotalCount,
			"workers_registered": status.WorkersRegistered,
			"details":            details,
		})
	}
}

// DestroyCloudInstance tears down cloud infrastructure
// @Summary Destroy cloud instance
// @Description Destroy a specific cloud infrastructure and remove its state
// @Tags Cloud
// @Produce json
// @Param id path string true "Infrastructure ID"
// @Success 200 {object} map[string]interface{} "Infrastructure destroyed"
// @Failure 404 {object} map[string]interface{} "Infrastructure not found"
// @Failure 500 {object} map[string]interface{} "Destroy failed"
// @Security BearerAuth
// @Router /osm/api/cloud/instances/{id} [delete]
func DestroyCloudInstance(cfg *config.Config) fiber.Handler {
	return func(c *fiber.Ctx) error {
		infraID := c.Params("id")

		cloudCfg, err := loadCloudConfigFromCfg(cfg)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   true,
				"message": err.Error(),
			})
		}

		infra, err := cloud.LoadInfrastructureState(infraID, cloudCfg.State.Path)
		if err != nil {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"error":   true,
				"message": fmt.Sprintf("Infrastructure not found: %v", err),
			})
		}

		provider, err := cloud.CreateProvider(cloudCfg, infra.Provider)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error":   true,
				"message": fmt.Sprintf("Failed to create provider: %v", err),
			})
		}

		lm := cloud.NewLifecycleManager(cloudCfg, provider, nil)
		if err := lm.Destroy(c.Context(), infra); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error":   true,
				"message": fmt.Sprintf("Failed to destroy infrastructure: %v", err),
			})
		}

		return c.JSON(fiber.Map{
			"message":  "Infrastructure destroyed successfully",
			"infra_id": infraID,
		})
	}
}

// EstimateCloudCost returns a cost estimate for the given configuration
// @Summary Estimate cloud cost
// @Description Get a cost estimate for provisioning cloud infrastructure
// @Tags Cloud
// @Accept json
// @Produce json
// @Param request body EstimateCloudCostRequest true "Cost estimation parameters"
// @Success 200 {object} map[string]interface{} "Cost estimate"
// @Failure 400 {object} map[string]interface{} "Invalid request"
// @Security BearerAuth
// @Router /osm/api/cloud/estimate [post]
func EstimateCloudCost(cfg *config.Config) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var req EstimateCloudCostRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   true,
				"message": "Invalid request body",
			})
		}

		if req.Provider == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   true,
				"message": "provider is required",
			})
		}

		if req.InstanceCount < 1 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   true,
				"message": "instance_count must be at least 1",
			})
		}

		cloudCfg, err := loadCloudConfigFromCfg(cfg)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   true,
				"message": err.Error(),
			})
		}

		provider, err := cloud.CreateProvider(cloudCfg, cloud.ProviderType(req.Provider))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   true,
				"message": fmt.Sprintf("Failed to create provider %q: %v", req.Provider, err),
			})
		}

		estimate, err := provider.EstimateCost(cloud.ModeVM, req.InstanceCount)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error":   true,
				"message": fmt.Sprintf("Failed to estimate cost: %v", err),
			})
		}

		return c.JSON(fiber.Map{
			"provider":       req.Provider,
			"instance_count": req.InstanceCount,
			"hourly_cost":    estimate.HourlyCost,
			"daily_cost":     estimate.DailyCost,
			"currency":       estimate.Currency,
			"breakdown":      estimate.Breakdown,
			"notes":          estimate.Notes,
		})
	}
}

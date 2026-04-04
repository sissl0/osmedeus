package cloud

import (
	"context"
	"fmt"
	"strconv"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws"
	awsec2 "github.com/pulumi/pulumi-aws/sdk/v6/go/aws/ec2"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// AWSProvider implements the Provider interface for AWS
type AWSProvider struct {
	accessKeyID     string
	secretAccessKey string
	region          string
	instanceType    string
	ami             string
	amiFilter       string
	useSpot         bool
}

// NewAWSProvider creates a new AWS provider
func NewAWSProvider(accessKeyID, secretAccessKey, region, instanceType, ami, amiFilter string, useSpot bool) (*AWSProvider, error) {
	if accessKeyID == "" {
		return nil, fmt.Errorf("AWS access key ID is required")
	}
	if secretAccessKey == "" {
		return nil, fmt.Errorf("AWS secret access key is required")
	}

	if region == "" {
		region = "us-east-1"
	}
	if instanceType == "" {
		instanceType = "t3.medium"
	}

	if amiFilter == "" {
		amiFilter = "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"
	}

	return &AWSProvider{
		accessKeyID:     accessKeyID,
		secretAccessKey: secretAccessKey,
		region:          region,
		instanceType:    instanceType,
		ami:             ami,
		amiFilter:       amiFilter,
		useSpot:         useSpot,
	}, nil
}

// Validate checks if the provider configuration is valid
func (p *AWSProvider) Validate(ctx context.Context) error {
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(p.region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(p.accessKeyID, p.secretAccessKey, "")),
	)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	_, err = sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("failed to validate AWS credentials: %w", err)
	}

	return nil
}

// EstimateCost estimates the cost for the given configuration
func (p *AWSProvider) EstimateCost(mode ExecutionMode, instanceCount int) (*CostEstimate, error) {
	if mode != ModeVM {
		return nil, fmt.Errorf("only VM mode is supported for AWS")
	}

	// Default pricing for common instance types (USD per hour)
	pricing := map[string]float64{
		"t3.micro":  0.0104,
		"t3.small":  0.0208,
		"t3.medium": 0.0416,
		"t3.large":  0.0832,
		"t3.xlarge": 0.1664,
		"m5.large":  0.0960,
		"m5.xlarge": 0.1920,
		"c5.large":  0.0850,
		"c5.xlarge": 0.1700,
	}

	hourlyRate, ok := pricing[p.instanceType]
	if !ok {
		// Default to t3.medium pricing if unknown
		hourlyRate = 0.0416
	}

	notes := []string{
		fmt.Sprintf("%d x %s instances @ $%.4f/hr each", instanceCount, p.instanceType, hourlyRate),
	}

	if p.useSpot {
		hourlyRate *= 0.4 // 60% discount for spot instances
		notes = append(notes, "Using spot instances (estimated 60% discount)")
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

// CreateInfrastructure provisions AWS EC2 instances
func (p *AWSProvider) CreateInfrastructure(ctx context.Context, opts *CreateOptions) (*Infrastructure, error) {
	infraID := fmt.Sprintf("cloud-aws-%d", time.Now().Unix())
	statePath := opts.StatePath

	pm, err := NewPulumiManager("osmedeus-cloud", infraID, statePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create Pulumi manager: %w", err)
	}

	// Set provider credentials
	if err := pm.SetConfig(ctx, "aws:region", p.region, false); err != nil {
		return nil, fmt.Errorf("failed to set AWS region: %w", err)
	}
	if err := pm.SetConfig(ctx, "aws:accessKey", p.accessKeyID, true); err != nil {
		return nil, fmt.Errorf("failed to set AWS access key: %w", err)
	}
	if err := pm.SetConfig(ctx, "aws:secretKey", p.secretAccessKey, true); err != nil {
		return nil, fmt.Errorf("failed to set AWS secret key: %w", err)
	}

	// Run Pulumi program
	if err := pm.Up(ctx, p.createInstanceProgram(infraID, opts)); err != nil {
		return nil, fmt.Errorf("failed to provision EC2 instances: %w", err)
	}

	// Extract outputs
	outputs, err := pm.GetOutputs(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get outputs: %w", err)
	}

	infra := &Infrastructure{
		ID:            infraID,
		Provider:      ProviderAWS,
		Mode:          opts.Mode,
		CreatedAt:     time.Now(),
		PulumiStackID: pm.GetStackName(),
		StatePath:     statePath,
		Resources:     buildResourcesFromOutputs(infraID, opts.InstanceCount, outputs),
		Metadata: map[string]interface{}{
			"region":        p.region,
			"instance_type": p.instanceType,
			"ssh_user":      "ubuntu",
		},
	}

	return infra, nil
}

// DestroyInfrastructure tears down AWS resources
func (p *AWSProvider) DestroyInfrastructure(ctx context.Context, infra *Infrastructure) error {
	statePath := infra.StatePath

	pm, err := NewPulumiManager("osmedeus-cloud", infra.PulumiStackID, statePath)
	if err != nil {
		return fmt.Errorf("failed to create Pulumi manager: %w", err)
	}

	if err := pm.SetConfig(ctx, "aws:region", p.region, false); err != nil {
		return fmt.Errorf("failed to set AWS region: %w", err)
	}
	if err := pm.SetConfig(ctx, "aws:accessKey", p.accessKeyID, true); err != nil {
		return fmt.Errorf("failed to set AWS access key: %w", err)
	}
	if err := pm.SetConfig(ctx, "aws:secretKey", p.secretAccessKey, true); err != nil {
		return fmt.Errorf("failed to set AWS secret key: %w", err)
	}

	return pm.Destroy(ctx)
}

// GetStatus retrieves the current status of infrastructure
func (p *AWSProvider) GetStatus(ctx context.Context, infra *Infrastructure) (*InfraStatus, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(p.region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(p.accessKeyID, p.secretAccessKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	ec2Client := ec2.NewFromConfig(cfg)

	status := &InfraStatus{
		Status:     "running",
		TotalCount: len(infra.Resources),
		Details:    make([]ResourceStatus, 0, len(infra.Resources)),
	}

	// Collect instance IDs
	instanceIDs := make([]string, 0, len(infra.Resources))
	for _, res := range infra.Resources {
		if res.ID != "" {
			instanceIDs = append(instanceIDs, res.ID)
		}
	}

	if len(instanceIDs) == 0 {
		return status, nil
	}

	// Describe instances
	describeOutput, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: instanceIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to describe instances: %w", err)
	}

	// Build a map of instance ID -> state
	instanceStates := make(map[string]ec2types.InstanceStateName)
	for _, reservation := range describeOutput.Reservations {
		for _, instance := range reservation.Instances {
			if instance.InstanceId != nil && instance.State != nil {
				instanceStates[*instance.InstanceId] = instance.State.Name
			}
		}
	}

	for _, res := range infra.Resources {
		rs := ResourceStatus{
			ResourceID:       res.ID,
			WorkerRegistered: res.WorkerID != "",
		}

		if res.WorkerID != "" {
			status.WorkersRegistered++
		}

		if state, ok := instanceStates[res.ID]; ok {
			rs.Status = string(state)
			if state == ec2types.InstanceStateNameRunning {
				status.ReadyCount++
			}
		} else {
			rs.Status = "unknown"
			rs.Message = "instance not found"
		}

		status.Details = append(status.Details, rs)
	}

	return status, nil
}

// Type returns the provider type
func (p *AWSProvider) Type() ProviderType {
	return ProviderAWS
}

// createInstanceProgram creates a Pulumi program for AWS EC2 instances
func (p *AWSProvider) createInstanceProgram(infraID string, opts *CreateOptions) pulumi.RunFunc {
	suffix := infraSuffix(infraID)

	return func(ctx *pulumi.Context) error {

		userData := GenerateCloudInit(opts.RedisURL, opts.SSHPublicKey, opts.SetupCommands)

		// Determine AMI
		ami := p.ami
		if opts.ImageID != "" {
			ami = opts.ImageID
		}
		if ami == "" {
			// Look up AMI using the configured filter
			mostRecent := true
			amiLookup, err := awsec2.LookupAmi(ctx, &awsec2.LookupAmiArgs{
				Filters: []awsec2.GetAmiFilter{
					{
						Name:   "name",
						Values: []string{p.amiFilter},
					},
					{
						Name:   "virtualization-type",
						Values: []string{"hvm"},
					},
				},
				Owners:     []string{"099720109477"}, // Canonical
				MostRecent: &mostRecent,
			})
			if err != nil {
				return fmt.Errorf("failed to look up Ubuntu AMI: %w", err)
			}
			ami = amiLookup.Id
		}

		// Use aws.GetRegion to reference the aws provider package
		regionResult, err := aws.GetRegion(ctx, &aws.GetRegionArgs{})
		if err != nil {
			return fmt.Errorf("failed to get AWS region: %w", err)
		}

		// Create security group allowing SSH inbound and all outbound
		sg, err := awsec2.NewSecurityGroup(ctx, "osmedeus-sg", &awsec2.SecurityGroupArgs{
			Description: pulumi.String("Osmedeus worker security group"),
			Ingress: awsec2.SecurityGroupIngressArray{
				&awsec2.SecurityGroupIngressArgs{
					Protocol:   pulumi.String("tcp"),
					FromPort:   pulumi.Int(22),
					ToPort:     pulumi.Int(22),
					CidrBlocks: pulumi.StringArray{pulumi.String("0.0.0.0/0")},
				},
			},
			Egress: awsec2.SecurityGroupEgressArray{
				&awsec2.SecurityGroupEgressArgs{
					Protocol:   pulumi.String("-1"),
					FromPort:   pulumi.Int(0),
					ToPort:     pulumi.Int(0),
					CidrBlocks: pulumi.StringArray{pulumi.String("0.0.0.0/0")},
				},
			},
			Tags: pulumi.StringMap{
				"Name":   pulumi.String(fmt.Sprintf("osmedeus-workers-%s", suffix)),
				"Region": pulumi.String(regionResult.Name),
			},
		})
		if err != nil {
			return fmt.Errorf("failed to create security group: %w", err)
		}

		// Create key pair
		keyPair, err := awsec2.NewKeyPair(ctx, "osmedeus-key", &awsec2.KeyPairArgs{
			KeyName:   pulumi.String(fmt.Sprintf("osmedeus-key-%s", suffix)),
			PublicKey: pulumi.String(opts.SSHPublicKey),
		})
		if err != nil {
			return fmt.Errorf("failed to create key pair: %w", err)
		}

		// Create instances
		for i := 0; i < opts.InstanceCount; i++ {
			instanceName := fmt.Sprintf("osmw-%s-%d", suffix, i)

			instance, err := awsec2.NewInstance(ctx, instanceName, &awsec2.InstanceArgs{
				Ami:                      pulumi.String(ami),
				InstanceType:             pulumi.String(p.instanceType),
				KeyName:                  keyPair.KeyName,
				VpcSecurityGroupIds:      pulumi.StringArray{sg.ID()},
				UserData:                 pulumi.String(userData),
				AssociatePublicIpAddress: pulumi.Bool(true),
				Tags: pulumi.StringMap{
					"Name":    pulumi.String(instanceName),
					"Project": pulumi.String("osmedeus"),
					"Role":    pulumi.String("worker"),
					"Index":   pulumi.String(strconv.Itoa(i)),
				},
			})
			if err != nil {
				return fmt.Errorf("failed to create instance %s: %w", instanceName, err)
			}

			ctx.Export(fmt.Sprintf("worker-%d-ip", i), instance.PublicIp)
			ctx.Export(fmt.Sprintf("worker-%d-id", i), instance.ID())
		}

		return nil
	}
}

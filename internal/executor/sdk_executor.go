package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	agnostic "github.com/j3ssie/go-agent-agnostic"
	"github.com/j3ssie/go-agent-agnostic/sdk/claude"
	"github.com/j3ssie/go-agent-agnostic/sdk/codex"
	"github.com/j3ssie/go-agent-agnostic/sdk/opencode"
	"github.com/j3ssie/osmedeus/v5/internal/core"
	oslogger "github.com/j3ssie/osmedeus/v5/internal/logger"
	"github.com/j3ssie/osmedeus/v5/internal/template"
	"go.uber.org/zap"
)

// supportedSDKAgents lists agent names supported by the go-agent-agnostic SDK.
var supportedSDKAgents = []string{"claude-code", "codex", "opencode"}

// SDKExecutor implements StepExecutorPlugin for agent-sdk steps.
// It uses the go-agent-agnostic library to run coding agents.
type SDKExecutor struct {
	templateEngine template.TemplateEngine
}

// NewSDKExecutor creates a new SDK executor.
func NewSDKExecutor(engine template.TemplateEngine) *SDKExecutor {
	return &SDKExecutor{
		templateEngine: engine,
	}
}

// Name returns the executor name for logging/debugging.
func (e *SDKExecutor) Name() string {
	return "agent-sdk"
}

// StepTypes returns the step types this executor handles.
func (e *SDKExecutor) StepTypes() []core.StepType {
	return []core.StepType{core.StepTypeAgentSDK}
}

// Execute runs an agent-sdk step.
func (e *SDKExecutor) Execute(ctx context.Context, step *core.Step, execCtx *core.ExecutionContext) (*core.StepResult, error) {
	result := &core.StepResult{
		StepName:  step.Name,
		Status:    core.StepStatusRunning,
		StartTime: time.Now(),
		Exports:   make(map[string]interface{}),
	}

	prompt := BuildPrompt(step)
	if prompt == "" {
		err := fmt.Errorf("agent-sdk step has no prompt (messages with content required)")
		e.fillResult(result, "", "", err)
		return result, err
	}

	agentName := step.Agent
	if agentName == "" {
		agentName = core.DefaultSDKAgent
	}

	opts := e.buildOptions(step, execCtx)

	strategy := ""
	var agentNames []string
	if step.SDKConfig != nil && step.SDKConfig.Strategy != "" && len(step.SDKConfig.Agents) > 0 {
		strategy = step.SDKConfig.Strategy
		agentNames = step.SDKConfig.Agents
	}

	log := oslogger.Get()

	if strategy != "" {
		// Multi-agent mode
		log.Debug("running agent-sdk in multi-agent mode",
			zap.String("strategy", strategy),
			zap.Strings("agents", agentNames))

		output, agentUsed, err := e.runMultiAgent(ctx, prompt, strategy, agentNames, opts)
		e.fillResult(result, output, agentUsed, err)
		return result, err
	}

	log.Debug("running agent-sdk",
		zap.String("agent", agentName),
		zap.Int("promptLength", len(prompt)))

	agent, err := e.createAgent(agentName, opts)
	if err != nil {
		e.fillResult(result, "", "", err)
		return result, err
	}
	defer func() { _ = agent.Close() }()

	output, err := agent.Run(ctx, prompt, nil)

	// Capture session ID if available
	sessionID := ""
	if sp, ok := agent.(agnostic.SessionProvider); ok {
		sessionID = sp.LastSessionID()
	}

	e.fillResult(result, output, agentName, err)
	result.Exports["sdk_session_id"] = sessionID

	return result, err
}

// buildOptions constructs agnostic.Options from step configuration.
func (e *SDKExecutor) buildOptions(step *core.Step, execCtx *core.ExecutionContext) *agnostic.Options {
	opts := &agnostic.Options{
		Cwd: step.Cwd,
	}
	if opts.Cwd == "" {
		opts.Cwd = execCtx.WorkspacePath
	}

	// Extract system prompt from messages
	for _, msg := range step.Messages {
		if msg.Role == "system" {
			if content, ok := msg.Content.(string); ok {
				opts.SystemPrompt = content
				break
			}
		}
	}

	cfg := step.SDKConfig
	if cfg == nil {
		return opts
	}

	opts.Model = cfg.Model
	opts.Env = cfg.Env

	// Apply agent-specific options
	agentName := step.Agent
	if agentName == "" {
		agentName = core.DefaultSDKAgent
	}

	switch agentName {
	case "claude-code":
		cc := &agnostic.ClaudeCodeOptions{}
		if cfg.MaxTurns > 0 {
			cc.MaxTurns = cfg.MaxTurns
		}
		if cfg.PermissionMode != "" {
			cc.PermissionMode = cfg.PermissionMode
		}
		if cfg.SessionResume != "" {
			cc.Resume = cfg.SessionResume
		}
		if len(step.AllowedPaths) > 0 {
			cc.AdditionalDirs = step.AllowedPaths
		}
		opts.ClaudeCode = cc

	case "codex":
		cx := &agnostic.CodexOptions{}
		if cfg.Sandbox != "" {
			cx.Sandbox = cfg.Sandbox
		}
		opts.Codex = cx

	case "opencode":
		opts.OpenCode = &agnostic.OpenCodeOptions{}
	}

	return opts
}

// createAgent creates an agnostic.Agent for the given agent name.
func (e *SDKExecutor) createAgent(name string, opts *agnostic.Options) (agnostic.Agent, error) {
	switch name {
	case "claude-code":
		return claude.New(opts), nil
	case "codex":
		return codex.New(opts), nil
	case "opencode":
		return opencode.New(opts), nil
	default:
		return nil, fmt.Errorf("unknown agent-sdk agent: %q (available: %s)", name, availableSDKAgentNames())
	}
}

// runMultiAgent runs multiple agents using the specified strategy.
func (e *SDKExecutor) runMultiAgent(ctx context.Context, prompt, strategy string, agentNames []string, baseOpts *agnostic.Options) (string, string, error) {
	agents := make([]agnostic.Agent, 0, len(agentNames))
	for _, name := range agentNames {
		a, err := e.createAgent(name, baseOpts)
		if err != nil {
			// Close already-created agents
			for _, created := range agents {
				_ = created.Close()
			}
			return "", "", fmt.Errorf("failed to create agent %q: %w", name, err)
		}
		agents = append(agents, a)
	}
	defer func() {
		for _, a := range agents {
			_ = a.Close()
		}
	}()

	switch strategy {
	case "first":
		result, err := agnostic.RunFirst(ctx, prompt, agents...)
		if err != nil {
			return "", "", err
		}
		return result.Output, string(result.Agent), nil

	case "all":
		results := agnostic.RunAll(ctx, prompt, agents...)
		var outputs []string
		var agentsUsed []string
		for _, r := range results {
			if r.Err == nil {
				outputs = append(outputs, r.Output)
				agentsUsed = append(agentsUsed, string(r.Agent))
			}
		}
		if len(outputs) == 0 {
			return "", "", fmt.Errorf("all %d agents failed", len(results))
		}
		return strings.Join(outputs, "\n---\n"), strings.Join(agentsUsed, ","), nil

	default:
		return "", "", fmt.Errorf("unknown multi-agent strategy: %q (use \"first\" or \"all\")", strategy)
	}
}

// fillResult populates a StepResult with agent output.
func (e *SDKExecutor) fillResult(result *core.StepResult, output, agentName string, err error) {
	result.EndTime = time.Now()
	result.Duration = result.EndTime.Sub(result.StartTime)
	result.Output = output
	result.Exports["sdk_output"] = output
	result.Exports["sdk_agent"] = agentName

	if err != nil {
		result.Status = core.StepStatusFailed
		result.Error = err
	} else {
		result.Status = core.StepStatusSuccess
	}
}

// ListSDKAgentNames returns the names of all supported SDK agents.
func ListSDKAgentNames() []string {
	return supportedSDKAgents
}

// availableSDKAgentNames returns a comma-separated list of supported SDK agent names.
func availableSDKAgentNames() string {
	return strings.Join(ListSDKAgentNames(), ", ")
}

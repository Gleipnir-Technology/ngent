package hermes

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/beyond5959/ngent/internal/agents"
	"github.com/beyond5959/ngent/internal/agents/acpcli"
	"github.com/beyond5959/ngent/internal/agents/acpstdio"
	"github.com/beyond5959/ngent/internal/agents/agentutil"
)

// hermesEnv builds the process environment with HERMES_HOME pointing to
// the user's real home. The Nix wrapper may set HERMES_HOME to a system
// path (/var/lib/hermes) that is inaccessible; we override it with
// $HOME/.hermes so the ACP adapter can read config and credentials.
func hermesEnv() []string {
	home := os.Getenv("HOME")
	if home == "" {
		if dir, err := os.UserHomeDir(); err == nil {
			home = dir
		}
	}
	hermesHome := filepath.Join(home, ".hermes")

	env := os.Environ()
	replaced := false
	for i, entry := range env {
		if strings.HasPrefix(entry, "HERMES_HOME=") {
			env[i] = "HERMES_HOME=" + hermesHome
			replaced = true
			break
		}
	}
	if !replaced && home != "" {
		env = append(env, "HERMES_HOME="+hermesHome)
	}
	return env
}

var handlePermissionRequest = acpcli.StructuredPermissionRequestHandler(acpcli.DefaultPermissionTimeout)

// Config configures the Hermes CLI ACP stdio provider.
type Config = agentutil.Config

type commandSpec struct {
	command string
	args    []string
	label   string
}

func commandCandidates() []commandSpec {
	return []commandSpec{
		{command: "hermes-acp", args: nil, label: "hermes-acp"},
		{command: agents.AgentIDHermes, args: []string{"acp"}, label: "hermes acp"},
		{command: agents.AgentIDHermes, args: []string{"--acp"}, label: "hermes --acp"},
		{command: agents.AgentIDHermes, args: []string{"--experimental-acp"}, label: "hermes --experimental-acp"},
	}
}

// Client runs one hermes process per ACP operation.
type Client struct {
	*acpcli.Client
}

var _ agents.Streamer = (*Client)(nil)
var _ agents.ConfigOptionManager = (*Client)(nil)
var _ agents.SessionLister = (*Client)(nil)
var _ agents.SessionTranscriptLoader = (*Client)(nil)
var _ agents.SlashCommandsProvider = (*Client)(nil)

// New constructs a Hermes ACP client.
func New(cfg Config) (*Client, error) {
	base, err := acpcli.New(agents.AgentIDHermes, cfg, acpcli.Hooks{
		OpenConn:                openConn(cfg.Dir),
		SessionNewParams:        acpcli.SessionNewParams(cfg.Dir),
		SessionLoadParams:       acpcli.SessionLoadParams(cfg.Dir),
		SessionListParams:       acpcli.SessionListParams(cfg.Dir),
		PromptParams:            promptParams,
		DiscoverModelsParams:    acpcli.DiscoverModelsParams(cfg.Dir),
		HandlePermissionRequest: handlePermissionRequest,
		Cancel:                  cancelWithNotify,
	})
	if err != nil {
		return nil, err
	}
	return &Client{Client: base}, nil
}

// Preflight checks that the hermes-acp binary is available in PATH.
func Preflight() error {
	if err := agentutil.PreflightBinary("hermes-acp"); err == nil {
		return nil
	}
	return agentutil.PreflightBinary(agents.AgentIDHermes)
}

func openConn(dir string) func(context.Context, acpcli.OpenConnRequest) (*acpstdio.Conn, func(), json.RawMessage, error) {
	return func(
		ctx context.Context,
		req acpcli.OpenConnRequest,
	) (*acpstdio.Conn, func(), json.RawMessage, error) {
		dir = strings.TrimSpace(dir)
		candidates := commandCandidates()

		var lastErr error
		for idx, spec := range candidates {
			conn, cleanup, initResult, err := acpcli.OpenProcess(ctx, acpcli.ProcessConfig{
				Command: spec.command,
				Args:    spec.args,
				Dir:     dir,
				Env:     hermesEnv(),
				ConnOptions: acpstdio.ConnOptions{
					Prefix:           agents.AgentIDHermes,
					AllowStdoutNoise: true,
				},
				InitializeParams: initializeParams(),
			})
			if err == nil {
				return conn, cleanup, initResult, nil
			}

			lastErr = err
			if idx < len(candidates)-1 && isACPStartupError(err) {
				continue
			}
			break
		}

		return nil, nil, nil, acpcli.WrapOpenError(agents.AgentIDHermes, req.Purpose, lastErr)
	}
}

func isACPStartupError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(msg, "connection closed") ||
		strings.Contains(msg, "decode rpc line") ||
		strings.Contains(msg, "exit status") ||
		strings.Contains(msg, "initialize:")
}

func initializeParams() map[string]any {
	return map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs": map[string]any{
				"readTextFile":  false,
				"writeTextFile": false,
			},
		},
	}
}

func promptParams(sessionID string, prompt agents.Prompt, modelID string) map[string]any {
	params := acpcli.ACPPromptParams(sessionID, prompt)
	if modelID = strings.TrimSpace(modelID); modelID != "" {
		params["model"] = modelID
	}
	return params
}

func cancelWithNotify(conn *acpstdio.Conn, sessionID string) {
	if conn == nil {
		return
	}
	conn.Notify("session/cancel", map[string]any{"sessionId": strings.TrimSpace(sessionID)})
}

// Name returns the provider identifier.
func (c *Client) Name() string {
	if c == nil || c.Client == nil {
		return agents.AgentIDHermes
	}
	return c.Client.Name()
}

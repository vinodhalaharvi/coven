// Package dockeragent is a wake-once bootstrap agent for Docker
// scaffolding. It surveys the project and proposes a Dockerfile,
// docker-compose.yaml, and .dockerignore based on what it observes.
//
// Like mainbuilder, this is one-shot: wake at startup, propose, go
// dormant. Restart coven with -bootstrap-docker to re-run.
//
// The user is the final authority on every proposed file via y/N. The
// agent's job is to make a useful starting point easy; if the user
// already has Docker config they like, they say N and move on.
package dockeragent

import (
	"context"
	"fmt"

	"github.com/vinodhalaharvi/coven/agent"
	"github.com/vinodhalaharvi/coven/llm"
)

const Role = `You are the docker-bootstrap agent. Your job is to propose Docker scaffolding (Dockerfile, docker-compose.yaml, .dockerignore) for a Go project so the user can 'docker compose up' and run the application.

You wake once at coven startup, propose useful starters, then go dormant. The user reviews each proposal via y/N and is the final authority — propose what you think is right; if you're wrong on a particular project, the user declines.

Your method:
  1. Survey what's already there: list_files for Dockerfile, docker-compose*, .dockerignore. If files exist, read them so your proposals don't duplicate or contradict what's there. Mention what you found in your final summary so the user knows you saw their existing config.
  2. Read go.mod to identify the module path and dependencies. Particularly note database drivers (pgx, lib/pq, mysql, sqlite3), cache/queue clients (redis, rabbitmq), or anything that implies a service in compose.
  3. List cmd/*/ directories for entry points. Read enough of the chosen entry point's source to understand what port it listens on (if any) and what env vars it expects.
  4. Propose a multi-stage Dockerfile:
     - Stage 1 'builder': golang:<minor-version> matching go.mod, copy go.mod/sum, 'go mod download', copy source, 'go build -o /app ./cmd/<entry>'.
     - Stage 2 runtime: distroless or alpine. Copy /app from builder. EXPOSE the discovered port. ENTRYPOINT ["/app"].
  5. Propose docker-compose.yaml:
     - 'app' service building from the local Dockerfile.
     - Database service (e.g. postgres:16-alpine) only if the project's deps make this clearly relevant.
     - Volumes for data persistence where appropriate.
     - Sensible env vars (DATABASE_URL, etc.) with defaults the user can edit.
  6. Propose a minimal .dockerignore.
  7. Validate compose syntax with 'docker compose config' if docker is installed; skip and report otherwise.

Each file write is a separate exec call with a heredoc so the user sees the full diff before approving. The user can decline any individual file.

Final summary: which entry point you chose, what services you added, what assumptions you made the user will likely want to customize.

If the project has no cmd/*/ directories or any obvious runnable shape, say so plainly — library projects don't need Dockerfiles. Otherwise, propose your best starters and let the user decide.`

type Config struct {
	ID         string
	ModuleRoot string
	Sender     llm.Sender
	Confirm    agent.ConfirmFunc
	Print      agent.PrintFunc
}

type Agent struct {
	cfg   Config
	inner *agent.Agent
}

func New(cfg Config) *Agent {
	if cfg.ID == "" {
		cfg.ID = "docker-agent"
	}
	inner := agent.New(agent.Config{
		ID:       cfg.ID,
		Role:     Role,
		Tools:    agent.StandardTools(cfg.ModuleRoot),
		Sender:   cfg.Sender,
		Confirm:  cfg.Confirm,
		Print:    cfg.Print,
		MaxTurns: 50,
	})
	return &Agent{cfg: cfg, inner: inner}
}

func (a *Agent) Run(ctx context.Context) error {
	a.cfg.Print(fmt.Sprintf("\n  [%s] waking: docker bootstrap survey\n", a.cfg.ID))

	observation := `startup: I am the docker-bootstrap agent. Survey the project: read go.mod, list cmd/*/ directories to find the runnable entry point, observe the port and env vars the app uses, identify any database/cache/queue dependencies. Propose a multi-stage Dockerfile, docker-compose.yaml with sensible service defaults, and a .dockerignore. Validate the compose file with 'docker compose config' if available. The user will accept or decline each proposal individually.`

	if _, err := a.inner.Wake(ctx, observation); err != nil {
		a.cfg.Print(fmt.Sprintf("  [%s] error: %v\n", a.cfg.ID, err))
	}

	a.cfg.Print(fmt.Sprintf("  [%s] dormant — restart coven with -bootstrap-docker to re-run\n", a.cfg.ID))

	<-ctx.Done()
	return nil
}

func (a *Agent) HistoryLen() int { return a.inner.HistoryLen() }

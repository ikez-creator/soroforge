package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/store"
)

// The handlers mirror the CLI one-for-one. Each decodes a request, calls the
// shared service, and encodes the result — deliberately with no logic of its
// own, so the two surfaces cannot drift apart in behaviour.

// health reports that the process is up.
//
// It deliberately does not check Postgres or RPC connectivity: a liveness probe
// that fails when a downstream dependency blips causes an orchestrator to
// restart a process that was working fine.
func (h *handlers) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ready reports whether the dependencies SoroForge needs are usable.
//
// Unlike /health, this is allowed to fail: an instance whose database is down
// cannot record a deploy, and the right response is to take it out of rotation
// rather than restart it. The check itself lives behind the Readiness
// interface, so this handler knows only how to render the verdict; it does not
// import a store or an RPC client.
func (h *handlers) ready(w http.ResponseWriter, r *http.Request) {
	status := h.readiness.Check(r.Context())

	code := http.StatusOK
	if !status.Ready {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, status)
}

// deploy handles POST /v1/deploy.
func (h *handlers) deploy(w http.ResponseWriter, r *http.Request) {
	var req deploy.DeployRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Alias == "" {
		writeError(w, http.StatusBadRequest, "alias is required")
		return
	}

	result, err := h.svc.Deploy(r.Context(), req)
	if err != nil {
		h.log.Error("deploy failed", "alias", req.Alias, "network", req.Network, "error", err)
		writeError(w, statusForError(err), err.Error())
		return
	}

	// 200 rather than 201 for a dry run: nothing was created.
	status := http.StatusCreated
	if result.DryRun {
		status = http.StatusOK
	}
	writeJSON(w, status, result)
}

// upgrade handles POST /v1/upgrade.
func (h *handlers) upgrade(w http.ResponseWriter, r *http.Request) {
	var req deploy.UpgradeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Alias == "" {
		writeError(w, http.StatusBadRequest, "alias is required")
		return
	}

	result, err := h.svc.Upgrade(r.Context(), req)
	if err != nil {
		h.log.Error("upgrade failed", "alias", req.Alias, "network", req.Network, "error", err)
		writeError(w, statusForError(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// listContractsResponse wraps the list so the payload can gain fields later
// without becoming a breaking change for clients parsing a bare array.
type listContractsResponse struct {
	Contracts []store.Contract `json:"contracts"`
}

// listContracts handles GET /v1/contracts?network=.
func (h *handlers) listContracts(w http.ResponseWriter, r *http.Request) {
	network := r.URL.Query().Get("network")

	contracts, err := h.svc.List(r.Context(), network)
	if err != nil {
		writeError(w, statusForError(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, listContractsResponse{Contracts: contracts})
}

type historyResponse struct {
	Alias       string             `json:"alias"`
	Network     string             `json:"network"`
	Deployments []store.Deployment `json:"deployments"`
}

// history handles GET /v1/contracts/{network}/{alias}/history.
func (h *handlers) history(w http.ResponseWriter, r *http.Request) {
	network := chi.URLParam(r, "network")
	alias := chi.URLParam(r, "alias")

	deployments, err := h.svc.History(r.Context(), network, alias)
	if err != nil {
		writeError(w, statusForError(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, historyResponse{
		Alias:       alias,
		Network:     network,
		Deployments: deployments,
	})
}

// status handles GET /v1/contracts/{network}/{alias}/status.
//
// Drift is reported as a 200 with state "drift", not an error status: the check
// ran and produced an answer. Callers that want a non-zero signal should read
// the state field — mapping drift to a 5xx would make a working check look like
// a broken server.
func (h *handlers) status(w http.ResponseWriter, r *http.Request) {
	network := chi.URLParam(r, "network")
	alias := chi.URLParam(r, "alias")

	result, err := h.svc.Status(r.Context(), deploy.StatusRequest{
		Alias:   alias,
		Network: network,
	})
	if err != nil {
		writeError(w, statusForError(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// statusAll drift-checks every tracked contract on a network. Like status,
// drift is a 200: the check ran and the body carries the verdict in in_sync.
func (h *handlers) statusAll(w http.ResponseWriter, r *http.Request) {
	result, err := h.svc.StatusAll(r.Context(), chi.URLParam(r, "network"))
	if err != nil {
		writeError(w, statusForError(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

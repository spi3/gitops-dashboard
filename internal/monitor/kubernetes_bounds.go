package monitor

import (
	"context"
	"errors"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// kubernetesRequestTimeout bounds every individual Kubernetes API
	// request (workload Get, pod List) via rest.Config.Timeout.
	kubernetesRequestTimeout = 10 * time.Second

	// kubernetesMaxRequestsPerService is the most Kubernetes API requests a
	// single applicable service issues per check attempt: one workload Get,
	// followed only when pod-selection metadata is usable by one pod List.
	kubernetesMaxRequestsPerService = 2

	// kubernetesCycleMargin is headroom the check-phase budget adds on top
	// of the worst-case request time, for non-request work (status writes,
	// health mapping) within the phase.
	kubernetesCycleMargin = 2 * time.Second

	// kubernetesIntervalFloor and kubernetesIntervalCeiling clamp the
	// configured check interval before it caps the cycle budget, so a very
	// short or very long interval still yields a sane phase budget.
	kubernetesIntervalFloor   = 5 * time.Second
	kubernetesIntervalCeiling = 2 * time.Minute

	// kubernetesFailureWriteTimeout bounds the separate failure-row write
	// phase that follows a Kubernetes phase or request deadline. It is
	// intentionally outside the check-phase budget itself.
	kubernetesFailureWriteTimeout = 2 * time.Second
)

// kubernetesCycleBudgetFunc computes the context budget for one Kubernetes
// check-phase attempt. It is a Monitor field (see kubernetesCycleBudget) so
// tests can substitute millisecond budgets without touching the production
// per-request timeout or formula.
type kubernetesCycleBudgetFunc func(services []core.Service, configuredInterval time.Duration) time.Duration

// kubernetesClientFunc builds the dynamic client used for one Kubernetes
// target's check. It is a Monitor field (see kubernetesClient) so tests can
// substitute a fake or test-server-backed client without a live cluster.
type kubernetesClientFunc func(target config.KubernetesTarget) (dynamic.Interface, error)

// computeKubernetesCycleBudget bounds one Kubernetes check-phase attempt (a
// scheduled loop iteration or CheckAll's pass over a target) so a stalled or
// slow cluster can never wedge that target's checks permanently.
//
// An applicable service has Runtime == "kubernetes" and a kind gvrForKind
// accepts; every other service issues zero Kubernetes API requests and does
// not contribute to the budget. Each applicable service issues at most
// kubernetesMaxRequestsPerService requests, each bounded by
// kubernetesRequestTimeout. The budget is the margin plus that worst case,
// capped at the configured interval clamped to
// [kubernetesIntervalFloor, kubernetesIntervalCeiling]. With no applicable
// services the budget is just the margin.
func computeKubernetesCycleBudget(services []core.Service, configuredInterval time.Duration) time.Duration {
	applicable := countApplicableKubernetesServices(services)
	if applicable == 0 {
		return kubernetesCycleMargin
	}
	intervalCap := clampDuration(configuredInterval, kubernetesIntervalFloor, kubernetesIntervalCeiling)
	budget := kubernetesCycleMargin + time.Duration(applicable)*kubernetesMaxRequestsPerService*kubernetesRequestTimeout
	if budget > intervalCap {
		return intervalCap
	}
	return budget
}

func clampDuration(value, minimum, maximum time.Duration) time.Duration {
	switch {
	case value < minimum:
		return minimum
	case value > maximum:
		return maximum
	default:
		return value
	}
}

// isContextTerminal reports whether err reflects the request's context
// ending, either via an explicit deadline (the check-phase budget or the
// per-request rest.Config.Timeout) or cancellation (the parent context
// stopping, including mid-request). Both cases must abort a Kubernetes
// target check immediately instead of being treated as an ordinary
// per-service error.
func isContextTerminal(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

func isApplicableKubernetesService(service core.Service) bool {
	if service.Runtime != "kubernetes" {
		return false
	}
	_, ok := gvrForKind(service.Kind)
	return ok
}

func countApplicableKubernetesServices(services []core.Service) int {
	count := 0
	for _, service := range services {
		if isApplicableKubernetesService(service) {
			count++
		}
	}
	return count
}

func applicableKubernetesServices(services []core.Service) []core.Service {
	result := make([]core.Service, 0, len(services))
	for _, service := range services {
		if isApplicableKubernetesService(service) {
			result = append(result, service)
		}
	}
	return result
}

// kubernetesRESTConfig builds the REST config backing the Kubernetes
// dynamic client, with every request bounded by kubernetesRequestTimeout.
// Factored out from client construction so it is testable with a temporary
// kubeconfig, without a live cluster.
func kubernetesRESTConfig(kubeconfigPath, kubeContext string) (*rest.Config, error) {
	loadingRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeContext}
	clientCfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)
	restCfg, err := clientCfg.ClientConfig()
	if err != nil {
		return nil, err
	}
	restCfg.Timeout = kubernetesRequestTimeout
	return restCfg, nil
}

func productionKubernetesClient(target config.KubernetesTarget) (dynamic.Interface, error) {
	restCfg, err := kubernetesRESTConfig(target.Kubeconfig, target.Context)
	if err != nil {
		return nil, err
	}
	return dynamic.NewForConfig(restCfg)
}

// recordKubernetesTargetFailure persists HealthError/"monitor target check
// failed" for every applicable service after a Kubernetes phase or request
// deadline, overwriting any earlier results from the same failed attempt.
//
// It writes nothing when the parent context is itself canceled or has
// reached its own deadline: at that point there is no remaining time budget
// to spend on a generic write, so the attempt returns promptly and the next
// scheduled attempt is left to recover instead.
func (monitor Monitor) recordKubernetesTargetFailure(parent context.Context, target string, services []core.Service) {
	if parent.Err() != nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), kubernetesFailureWriteTimeout)
	defer cancel()
	now := time.Now().UTC()
	for _, service := range applicableKubernetesServices(services) {
		if err := monitor.store.UpsertStatus(writeCtx, core.StatusResult{
			ServiceID: service.ID,
			Target:    target,
			Health:    core.HealthError,
			Message:   "monitor target check failed",
			CheckedAt: now,
		}); err != nil {
			monitor.logger.Error("persist kubernetes target failure", "target", target, "service", service.ID, "error", err)
		}
	}
}

// handleKubernetesCheckFailure records the effect of a failed Kubernetes
// check attempt. A phase or request deadline routes through the bounded,
// parent-aware Kubernetes failure writer above; every other error (for
// example an invalid kubeconfig) falls back to the shared generic target
// failure writer used by the other runtimes, unless the parent context was
// canceled, in which case nothing is written.
func (monitor Monitor) handleKubernetesCheckFailure(parent context.Context, target string, err error, services []core.Service) {
	if errors.Is(err, context.DeadlineExceeded) {
		monitor.recordKubernetesTargetFailure(parent, target, services)
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	monitor.recordTargetFailure(parent, target, runtimeServices(services, "kubernetes"))
}

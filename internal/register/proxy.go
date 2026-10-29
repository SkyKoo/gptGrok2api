package register

import (
	"context"
	"errors"
	"strings"
	"time"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

type ProxyRouting struct{ Manager *proxyruntime.Manager }

// acquire journals the selected node before any mailbox or signup side effect.
// Recovery uses that journal even when the registration settings have changed.
func (r *ProxyRouting) acquire(ctx context.Context, id, platform string, selection *string, get func(string) (RegistrationJob, error), change func(string, func(*RegistrationJob)) error) (func(), error) {
	if r == nil {
		return func() {}, nil
	}
	job, err := get(id)
	if err != nil {
		return nil, err
	}
	lease, err := r.Manager.AcquireTask(ctx, *selection, platform, "registration", job.ProxyBinding)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fail("proxy", "binding_unavailable")
	}
	if err = change(id, func(j *RegistrationJob) {
		b := lease.Binding
		j.ProxyBinding = &b
		action := "proxy_selected"
		detail := "注册出口已固定："
		if job.ProxyBinding != nil {
			action = "proxy_reused"
			detail = "复用任务原出口："
		}
		appendRegistrationLog(j, action, detail+b.Label(), "info")
	}); err != nil {
		lease.Release()
		return nil, err
	}
	*selection = lease.URL
	if *selection == "" {
		*selection = "direct"
	}
	return lease.Release, nil
}

func attachProxyBinding(job *RegistrationJob, account map[string]any) {
	if job.ProxyBinding == nil {
		return
	}
	b := *job.ProxyBinding
	account["proxy_binding"] = b
	account["proxy"] = b.Reference
}

func (r *ProxyRouting) recordFailure(job *RegistrationJob, err error) {
	if r == nil || job.ProxyBinding == nil {
		return
	}
	message := safeFailure(err)
	var failure *Failure
	limited := errors.As(err, &failure) && (failure.HTTPStatus == 429 || failure.UpstreamStatus == 429)
	// Refusal is scoped to the platform/operation; a working network path is
	// not marked globally unhealthy. No alternate node is acquired here.
	if strings.Contains(message, "signup_unavailable") || strings.Contains(message, "turnstile_incomplete") || limited {
		job.ProxyBinding.BlockedUntil = time.Now().Add(15 * time.Minute)
		r.Manager.BlockTaskScope(*job.ProxyBinding, job.ProxyBinding.BlockedUntil)
	}
}

func (r *ProxyRouting) restore(jobs []RegistrationJob) {
	if r == nil {
		return
	}
	for _, j := range jobs {
		if j.ProxyBinding != nil && time.Now().Before(j.ProxyBinding.BlockedUntil) {
			r.Manager.BlockTaskScope(*j.ProxyBinding, j.ProxyBinding.BlockedUntil)
		}
	}
}
func (e *FreeEngine) SetProxyRouting(r *ProxyRouting) { e.ProxyRouting = r; r.restore(e.jobs) }
func (e *GrokEngine) SetProxyRouting(r *ProxyRouting) { e.ProxyRouting = r; r.restore(e.jobs) }

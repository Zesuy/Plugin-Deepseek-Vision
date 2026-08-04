package tracelog

import "context"

type sessionContextKey struct{}
type jobContextKey struct{}

type Job struct {
	ID           int
	ImageNumbers []int
}

func WithSession(ctx context.Context, session *Session) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if session == nil {
		return ctx
	}
	return context.WithValue(ctx, sessionContextKey{}, session)
}

func FromContext(ctx context.Context) *Session {
	if ctx == nil {
		return nil
	}
	session, _ := ctx.Value(sessionContextKey{}).(*Session)
	return session
}

func WithJob(ctx context.Context, job Job) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	job.ImageNumbers = append([]int(nil), job.ImageNumbers...)
	return context.WithValue(ctx, jobContextKey{}, job)
}

func JobFromContext(ctx context.Context) Job {
	if ctx == nil {
		return Job{}
	}
	job, _ := ctx.Value(jobContextKey{}).(Job)
	job.ImageNumbers = append([]int(nil), job.ImageNumbers...)
	return job
}

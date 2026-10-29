//go:build !linux

package files

import "context"

type RestoreSession struct{}

func (m *Manager) ProbeRestore(context.Context, RestoreParams) (RestoreProbe, error) {
	return RestoreProbe{}, ErrPlatform
}
func (m *Manager) BeginRestore(context.Context, RestoreParams) (*RestoreSession, error) {
	return nil, ErrPlatform
}
func (s *RestoreSession) Target() string { return "" }
func (s *RestoreSession) Inspect(context.Context) ([]RestoreEntry, error) {
	return nil, ErrPlatform
}
func (s *RestoreSession) Publish(context.Context, []RestoreEntry) (RestoreReceipt, error) {
	return RestoreReceipt{}, ErrPlatform
}
func (s *RestoreSession) Abort(context.Context) error { return nil }

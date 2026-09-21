package daemon

import (
	"context"
	"errors"
	"testing"

	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

type autoRootProvider struct {
	*fakeprovider.Fake
	listErrs    []error
	listCalls   int
	ensureErr   error
	ensureCalls int
}

func (p *autoRootProvider) List(context.Context, string, string) ([]provider.Entry, string, error) {
	p.listCalls++
	if len(p.listErrs) == 0 {
		return nil, "", nil
	}
	err := p.listErrs[0]
	p.listErrs = p.listErrs[1:]
	return nil, "", err
}

func (p *autoRootProvider) EnsureRoot(context.Context) error {
	p.ensureCalls++
	return p.ensureErr
}

type listOnlyProvider struct {
	*fakeprovider.Fake
	listCalls int
}

func (p *listOnlyProvider) List(context.Context, string, string) ([]provider.Entry, string, error) {
	p.listCalls++
	return nil, "", provider.ErrNotFound
}

func TestAccountCheckCreatesAMissingRootAndRetriesTheList(t *testing.T) {
	p := &autoRootProvider{
		Fake:     fakeprovider.New("auto-root"),
		listErrs: []error{provider.ErrNotFound, nil},
	}
	if err := checkAccountProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if p.ensureCalls != 1 || p.listCalls != 2 {
		t.Fatalf("ensure calls = %d, list calls = %d; want 1 and 2", p.ensureCalls, p.listCalls)
	}
}

func TestAccountCheckDoesNotCreateForOtherErrors(t *testing.T) {
	p := &autoRootProvider{
		Fake:     fakeprovider.New("auto-root"),
		listErrs: []error{provider.ErrAuth},
	}
	err := checkAccountProvider(context.Background(), p)
	if !errors.Is(err, provider.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	if p.ensureCalls != 0 || p.listCalls != 1 {
		t.Fatalf("ensure calls = %d, list calls = %d; want 0 and 1", p.ensureCalls, p.listCalls)
	}
}

func TestAccountCheckPreservesNotFoundForProvidersThatCannotCreateTheirRoot(t *testing.T) {
	p := &listOnlyProvider{Fake: fakeprovider.New("list-only")}
	err := checkAccountProvider(context.Background(), p)
	if !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if p.listCalls != 1 {
		t.Fatalf("list calls = %d, want 1", p.listCalls)
	}
}

func TestAccountCheckReturnsRootCreationFailureWithoutRelisting(t *testing.T) {
	want := errors.New("mkdir denied")
	p := &autoRootProvider{
		Fake:      fakeprovider.New("auto-root"),
		listErrs:  []error{provider.ErrNotFound},
		ensureErr: want,
	}
	if err := checkAccountProvider(context.Background(), p); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if p.ensureCalls != 1 || p.listCalls != 1 {
		t.Fatalf("ensure calls = %d, list calls = %d; want 1 and 1", p.ensureCalls, p.listCalls)
	}
}

package account

import (
	"testing"
)

func TestRegisterProviderFactory_LookupRoundTrip(t *testing.T) {
	t.Cleanup(resetProviderRegistry)
	resetProviderRegistry()

	called := false
	RegisterProviderFactory("fake", func(raw any) (AuthProvider, error) {
		called = true
		return nil, nil
	})
	f, ok := LookupProviderFactory("fake")
	if !ok {
		t.Fatal("expected factory to be found")
	}
	if _, err := f(nil); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("factory not invoked")
	}
}

func TestRegisterProviderFactory_DuplicatePanics(t *testing.T) {
	t.Cleanup(resetProviderRegistry)
	resetProviderRegistry()

	RegisterProviderFactory("dup", func(any) (AuthProvider, error) { return nil, nil })

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	RegisterProviderFactory("dup", func(any) (AuthProvider, error) { return nil, nil })
}

func TestRegisterProviderFactory_EmptyNamePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty name")
		}
	}()
	RegisterProviderFactory("", func(any) (AuthProvider, error) { return nil, nil })
}

func TestRegisterProviderFactory_NilFactoryPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil factory")
		}
	}()
	RegisterProviderFactory("nil", nil)
}

func TestLookupProviderFactory_Unknown(t *testing.T) {
	t.Cleanup(resetProviderRegistry)
	resetProviderRegistry()
	if _, ok := LookupProviderFactory("nope"); ok {
		t.Fatal("expected ok=false for unknown name")
	}
}

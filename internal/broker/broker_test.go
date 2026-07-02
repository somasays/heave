package broker

import (
	"errors"
	"testing"
)

// fakeStore records what the broker asks of the scope store and simulates admits.
type fakeStore struct {
	reserved   map[string]float64 // request-count per scope
	tokens     map[string]int
	denyAll    bool
	failErr    error
	lastMaxUSD []float64
	lastMaxTok []int
	settleDT   int
	released   bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{reserved: map[string]float64{}, tokens: map[string]int{}}
}

func (f *fakeStore) Reserve(keys, names []string, maxUSD []float64, maxTokens, _ []int, estUSD float64, estTokens int, _ string) (bool, string, string, error) {
	f.lastMaxUSD, f.lastMaxTok = maxUSD, maxTokens
	if f.failErr != nil {
		return true, "", "", f.failErr
	}
	if f.denyAll {
		return false, names[0], "velocity", nil
	}
	f.reserved[keys[0]] += estUSD
	f.tokens[keys[0]] += estTokens
	return true, "", "", nil
}

func (f *fakeStore) Settle(_ []string, _ float64, deltaTokens int) error {
	f.settleDT += deltaTokens
	return nil
}

func (f *fakeStore) Release(keys []string, _ string, estUSD float64, estTokens int, settled bool) error {
	f.released = true
	if !settled {
		f.reserved[keys[0]] -= estUSD
		f.tokens[keys[0]] -= estTokens
	}
	return nil
}

func TestBrokerInertWithoutStore(t *testing.T) {
	b := New(nil, map[string]Limit{"p": {RPM: 1}})
	if b.Active("p") {
		t.Fatal("no store → brokering must be inactive")
	}
	lease, admitted, _ := b.Reserve("p", 10)
	if !admitted || lease != nil {
		t.Fatalf("inactive broker must admit with no lease, got admitted=%v lease=%v", admitted, lease)
	}
}

func TestBrokerInertWithoutLimit(t *testing.T) {
	b := New(newFakeStore(), map[string]Limit{"other": {RPM: 1}})
	if b.Active("p") {
		t.Fatal("a provider with no configured limit is not brokered")
	}
	if _, admitted, _ := b.Reserve("p", 10); !admitted {
		t.Fatal("unlimited provider must admit")
	}
}

func TestBrokerReservesRPMandTPM(t *testing.T) {
	fs := newFakeStore()
	b := New(fs, map[string]Limit{"p": {RPM: 100, TPM: 1000}})
	lease, admitted, _ := b.Reserve("p", 50)
	if !admitted || lease == nil {
		t.Fatalf("under quota must admit with a lease, got admitted=%v lease=%v", admitted, lease)
	}
	// RPM maps to the count dimension (cap 100, reserve 1); TPM to tokens (cap 1000, reserve 50).
	if len(fs.lastMaxUSD) != 1 || fs.lastMaxUSD[0] != 100 || fs.lastMaxTok[0] != 1000 {
		t.Fatalf("caps mismatch: maxUSD=%v maxTok=%v", fs.lastMaxUSD, fs.lastMaxTok)
	}
	if fs.reserved["prov:p"] != 1 || fs.tokens["prov:p"] != 50 {
		t.Fatalf("must reserve 1 request + 50 tokens, got %v / %v", fs.reserved, fs.tokens)
	}
}

func TestBrokerDenyReturnsRetryAfter(t *testing.T) {
	fs := newFakeStore()
	fs.denyAll = true
	b := New(fs, map[string]Limit{"p": {RPM: 1}})
	lease, admitted, retry := b.Reserve("p", 10)
	if admitted || lease != nil || retry <= 0 {
		t.Fatalf("quota-full must deny with a retry hint and no lease, got admitted=%v retry=%d", admitted, retry)
	}
}

func TestBrokerFailsOpen(t *testing.T) {
	fs := newFakeStore()
	fs.failErr = errors.New("redis down")
	b := New(fs, map[string]Limit{"p": {RPM: 1}})
	lease, admitted, _ := b.Reserve("p", 10)
	if !admitted || lease != nil {
		t.Fatalf("a store error must fail OPEN with no lease, got admitted=%v lease=%v", admitted, lease)
	}
}

func TestBrokerLeaseSettleReconcilesTokens(t *testing.T) {
	fs := newFakeStore()
	b := New(fs, map[string]Limit{"p": {TPM: 1000}})
	lease, _, _ := b.Reserve("p", 50)
	lease.Settle(30) // actual fewer tokens than estimated
	if fs.settleDT != 30-50 {
		t.Fatalf("settle must reconcile tokens (actual-est), got %d", fs.settleDT)
	}
}

func TestBrokerLeaseReleaseFreesUnsettled(t *testing.T) {
	fs := newFakeStore()
	b := New(fs, map[string]Limit{"p": {RPM: 10, TPM: 1000}})
	lease, _, _ := b.Reserve("p", 50)
	lease.Release() // failed request → free the reservation
	if fs.reserved["prov:p"] != 0 || fs.tokens["prov:p"] != 0 {
		t.Fatalf("released reservation must free the quota, got %v / %v", fs.reserved, fs.tokens)
	}
}

func TestBrokerSettledReleaseKeepsCount(t *testing.T) {
	fs := newFakeStore()
	b := New(fs, map[string]Limit{"p": {RPM: 10}})
	lease, _, _ := b.Reserve("p", 0)
	lease.Settle(0) // success → the request legitimately counts
	lease.Release() // must NOT subtract the counted request
	if fs.reserved["prov:p"] != 1 {
		t.Fatalf("a settled request must stay counted after Release, got %v", fs.reserved["prov:p"])
	}
}

func TestBrokerFailOpenIsObservable(t *testing.T) {
	fs := newFakeStore()
	fs.failErr = errors.New("redis down")
	b := New(fs, map[string]Limit{"p": {RPM: 1}})
	for i := 0; i < 3; i++ {
		b.Reserve("p", 10)
	}
	if b.Degraded() != 3 {
		t.Fatalf("fail-open admits must be counted, got %d", b.Degraded())
	}
}

// TestBrokerSingleDimensionSkipsOther: an RPM-only limit must not write to the
// token window on settle (and vice versa), so no junk keys accrue.
func TestBrokerSingleDimensionSkipsOther(t *testing.T) {
	fs := newFakeStore()
	b := New(fs, map[string]Limit{"p": {RPM: 10}}) // RPM only, no TPM
	lease, _, _ := b.Reserve("p", 50)
	lease.Settle(999) // actual tokens irrelevant — TPM not configured
	if fs.settleDT != 0 {
		t.Fatalf("RPM-only limit must not settle a token delta, got %d", fs.settleDT)
	}
}

func TestBrokerLeaseNilSafe(t *testing.T) {
	var le *Lease
	le.Settle(1) // must not panic
	le.Release()
}

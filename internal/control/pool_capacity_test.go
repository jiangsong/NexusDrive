package control

import (
	"net/http"
	"testing"

	"cloudfs/internal/config"
)

// Placement ranks every member whose free space is known ahead of every member
// whose free space is not, so a drive that reports nothing loses each decision
// and slowly stops receiving files. The fix in the file is a `capacity:` on the
// member; the setup flow is where a person can be asked for it, while they are
// looking at the drive that could not answer.
func TestCreatingAPoolRecordsTheCapacityGivenForAMemberThatCannotReportIts(t *testing.T) {
	srv, _, path := accountsServer(t)
	rr := accountRequest(t, srv, http.MethodPost, "/pool/create", PoolCreateRequest{
		Name:           "home",
		Members:        []string{"existing"},
		Replicas:       1,
		MemberCapacity: map[string]int64{"existing": 2 << 40},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /pool/create = %d %s", rr.Code, rr.Body)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	members := cfg.Pools["home"].Members
	if len(members) != 1 {
		t.Fatalf("members = %+v", members)
	}
	if int64(members[0].Capacity) != 2<<40 {
		t.Fatalf("member capacity = %d, want the value supplied during setup", int64(members[0].Capacity))
	}
}

// A member that reports its own space needs no help, and writing a guessed
// capacity for it would override the truth the backend reports.
func TestCreatingAPoolLeavesCapacityUnsetForMembersThatWereNotGivenOne(t *testing.T) {
	srv, _, path := accountsServer(t)
	rr := accountRequest(t, srv, http.MethodPost, "/pool/create", PoolCreateRequest{
		Name: "home", Members: []string{"existing"}, Replicas: 1,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /pool/create = %d %s", rr.Code, rr.Body)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c := int64(cfg.Pools["home"].Members[0].Capacity); c != 0 {
		t.Fatalf("member capacity = %d, want it left unset", c)
	}
}

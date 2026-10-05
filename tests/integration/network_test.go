package integration

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestGate2TrafficCrossesTheRouter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	lab := newLab(t, labSpec("gate2"))
	_, client, err := lab.Client(0)
	if err != nil {
		t.Fatal(err)
	}
	_, server, err := lab.Server(0)
	if err != nil {
		t.Fatal(err)
	}

	stdout, _, err := lab.Send(ctx, client.Namespace,
		"ping", "-c", "3", "-W", "2", server.Address.Addr().String())
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Logf("client %s -> server %s\n%s", client.Address, server.Address, stdout)

	if !strings.Contains(stdout, " 0% packet loss") {
		t.Fatalf("the traffic did not arrive:\n%s", stdout)
	}
	if !strings.Contains(stdout, "ttl=63") {
		t.Errorf("the replies did not cross the router, ttl should be 63:\n%s", stdout)
	}
}

package session

import (
	"testing"

	"lingzhu/internal/event"
	"lingzhu/internal/permission"
)

func TestSessionPermissionsOverlay(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Append("s1", event.User("hello")); err != nil {
		t.Fatal(err)
	}
	rules := permission.Rules{
		{Action: "shell", Resource: "*", Effect: permission.Deny},
		{Action: "read", Resource: "*.env", Effect: permission.Ask},
	}
	if err := store.SetPermissions("s1", rules); err != nil {
		t.Fatal(err)
	}
	got, err := store.Permissions("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Action != "shell" || got[0].Effect != permission.Deny {
		t.Fatalf("permissions = %+v", got)
	}
	// A later replacement wins rather than appending.
	if err := store.SetPermissions("s1", permission.Rules{{Action: "edit", Resource: "*", Effect: permission.Deny}}); err != nil {
		t.Fatal(err)
	}
	got, err = store.Permissions("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Action != "edit" {
		t.Fatalf("replacement permissions = %+v", got)
	}
}

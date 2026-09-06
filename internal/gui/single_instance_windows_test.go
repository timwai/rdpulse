package gui

import "testing"

func TestSingleInstanceMutexRejectsSecondAcquire(t *testing.T) {
	name := `Local\RDPulse.Agent.GUI.test.` + t.Name()
	release, ok := tryAcquireNamedInstance(name)
	if !ok {
		t.Fatal("first acquire should succeed")
	}
	t.Cleanup(release)

	_, second := tryAcquireNamedInstance(name)
	if second {
		t.Fatal("second acquire should fail while the first instance still holds the mutex")
	}

	release()
	again, ok := tryAcquireNamedInstance(name)
	if !ok {
		t.Fatal("acquire should succeed after the first instance released")
	}
	again()
}

package payment

import "testing"

func TestBetaCodeVerify(t *testing.T) {
	authorizer := NewBetaCode(" VPN-BETA-2026 ")
	if !authorizer.Verify("VPN-BETA-2026") {
		t.Fatal("expected valid beta code")
	}
	if authorizer.Verify("wrong") {
		t.Fatal("wrong beta code was accepted")
	}
}

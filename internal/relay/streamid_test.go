package relay

import "testing"

func TestCleanStreamID(t *testing.T) {
	cases := map[string]string{
		"":                                "",
		"cam01":                           "cam01",
		"publish:cam01":                   "cam01",
		"read:cam01":                      "cam01",
		"publish:cam01:EOLPC:secret!":     "cam01",
		"publish:cam01:user:pass:query":   "cam01",
		"#!::r=cam01,m=publish":           "cam01",
		"#!::m=publish, r=cam01 ,u=EOLPC": "cam01",
		"#!::m=publish":                   "",
	}
	for in, want := range cases {
		if got := cleanStreamID(in); got != want {
			t.Errorf("cleanStreamID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidatePassphrase(t *testing.T) {
	for p, ok := range map[string]bool{
		"":                       true,
		"short":                  false,
		"exactly10!":             true,
		string(make([]byte, 79)): true,
		string(make([]byte, 80)): false,
	} {
		if err := ValidatePassphrase(p); (err == nil) != ok {
			t.Errorf("ValidatePassphrase(len %d) err=%v, want ok=%v", len(p), err, ok)
		}
	}
}

func TestAcceptsRoutedSender(t *testing.T) {
	open := &Stream{}
	prot := &Stream{}
	prot.setPassphrasesLocked("protected-pass", "")
	if !open.acceptsRoutedSender("") || !open.acceptsRoutedSender("anything-else") {
		t.Error("unprotected stream should accept routed senders")
	}
	if prot.acceptsRoutedSender("") {
		t.Error("protected stream must reject a sender from an unprotected port")
	}
	if prot.acceptsRoutedSender("other-passphrase") {
		t.Error("protected stream must reject a sender that proved a different passphrase")
	}
	if !prot.acceptsRoutedSender("protected-pass") {
		t.Error("protected stream should accept a sender that proved its passphrase")
	}
	if !prot.InProtected || prot.OutProtected {
		t.Error("protected flags not set correctly")
	}
}

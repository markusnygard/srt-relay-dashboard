package relay

import "fmt"

// SRT passphrase limits enforced by libsrt.
const (
	MinPassphraseLen = 10
	MaxPassphraseLen = 79
)

// ErrPassphraseLength is returned for a passphrase libsrt would reject.
var ErrPassphraseLength = fmt.Errorf("passphrase must be %d-%d characters", MinPassphraseLen, MaxPassphraseLen)

// ValidatePassphrase accepts "" (no encryption) or a libsrt-valid passphrase.
func ValidatePassphrase(p string) error {
	if p == "" {
		return nil
	}
	if len(p) < MinPassphraseLen || len(p) > MaxPassphraseLen {
		return ErrPassphraseLength
	}
	return nil
}

// listenerOptions copies base and adds the passphrase when one is set. With a
// passphrase on a listener, libsrt (enforced encryption, the default) only
// accepts callers that use the same passphrase.
func listenerOptions(base map[string]string, passphrase string) map[string]string {
	o := make(map[string]string, len(base)+1)
	for k, v := range base {
		o[k] = v
	}
	if passphrase != "" {
		o["passphrase"] = passphrase
	}
	return o
}

// Passphrases returns the stream's sender (ingress) and receiver (egress)
// passphrases. They are never part of the stream JSON.
func (s *Stream) Passphrases() (in, out string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inPassphrase, s.outPassphrase
}

// setPassphrasesLocked stores the passphrases and the public "protected"
// flags. Caller holds s.mu.
func (s *Stream) setPassphrasesLocked(in, out string) {
	s.inPassphrase, s.outPassphrase = in, out
	s.InProtected, s.OutProtected = in != "", out != ""
}

// SetPassphrases changes the sender and/or receiver passphrase: nil leaves a
// value unchanged, "" removes it. If anything changed, the listeners are
// rebuilt with the new settings and the connected sender and readers are
// dropped, so they have to reconnect with the new passphrase.
func (s *Stream) SetPassphrases(in, out *string) (changed bool, err error) {
	for _, p := range []*string{in, out} {
		if p != nil {
			if err := ValidatePassphrase(*p); err != nil {
				return false, err
			}
		}
	}
	s.mu.Lock()
	newIn, newOut := s.inPassphrase, s.outPassphrase
	if in != nil {
		newIn = *in
	}
	if out != nil {
		newOut = *out
	}
	changed = newIn != s.inPassphrase || newOut != s.outPassphrase
	session := s.ingressSession
	if changed {
		s.setPassphrasesLocked(newIn, newOut)
		s.restartListeners = true
	}
	s.mu.Unlock()

	if changed {
		s.logf("passphrases changed (sender protected=%v, receiver protected=%v); restarting listeners", newIn != "", newOut != "")
		if session != nil {
			session.Close() // the sender must reconnect with the new passphrase
		}
		s.update()
	}
	return changed, nil
}

// takeRestart reports (and clears) a pending listener restart.
func (s *Stream) takeRestart() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.restartListeners
	s.restartListeners = false
	return r
}

// acceptsRoutedSender reports whether a sender that was accepted on another
// stream's ingress listener (which checked that stream's passphrase,
// listenerPass) may be routed to s by streamid. A protected stream only takes
// senders that already proved its passphrase, so an unprotected port can't be
// used to bypass it.
func (s *Stream) acceptsRoutedSender(listenerPass string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inPassphrase == "" || s.inPassphrase == listenerPass
}

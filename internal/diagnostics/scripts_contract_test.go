package diagnostics

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// Pin the complete bundled content, including its preamble, to the published
// recipe digest. A fallback must never silently execute a different script.
func TestBundledRecipeDigests(t *testing.T) {
	expected := map[string]string{
		"containers": "74ded63821a860278d21d5e39ad0fcf76c7cb6d545735218ae09f9ae9b51c061",
		"cpu":        "d84560f461b327753bbd2262fd077e8bd53dd1e9cb9038fe1aee827af1ee650e",
		"databases":  "29e0d9451f2a9faca31ec4b9764c34e57d87cefb9039e223a5fb89d01e8cda69",
		"disk":       "a04e6aff341cecfb794f2659258751618722891b2007be022ef67cf69b60f4eb",
		"memory":     "6b52baa735a66c942789284421a482c034fcaf2f1fb03d6e98d6f954b733670e",
		"network":    "fd93e543aaa31e93fdbe49ff2b530f0c47fee9215fadae7fdef05ed8e6b15739",
		"nginx":      "269863e7138475607950f55485a155cfead6b030abbd17faef4995d896e119fd",
		"systemd":    "577b1a09d2b9d5b1897677545e6c7f2f0fa36d6dbbf95c334e8345918e056fb0",
	}
	for name, digest := range expected {
		sum := sha256.Sum256([]byte(scriptPreamble + scripts[name]))
		if hex.EncodeToString(sum[:]) != digest {
			t.Errorf("bundled recipe digest changed: %s", name)
		}
	}
}

// Command corpusgen regenerates the SOPS fixture corpus in testdata/sops/corpus
// (run it through `task sops:corpus`). It encrypts the plaintext sources in
// testdata/sops/plain to the TEST-ONLY recipients in testdata/sops/recipients.txt
// with the sops CLI pinned as a Go tool (`go tool sops`), then derives the
// tampered variants by scripted edits of the generated files.
//
// It never reads an age identity: encryption needs only public recipients.
// Re-running it reproduces the corpus' structure (recipients, key groups,
// encryption rules, type tags); ciphertexts, data keys and timestamps differ
// on every run. The inspection goldens in pkg/sops pin that structure.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// encryption is one corpus file produced by `sops encrypt`.
type encryption struct {
	out   string // file name in corpus/
	plain string // file name in plain/
	// rule is the body of the single creation rule (without path_regex),
	// with {name} placeholders for recipients.
	rule string
}

// tamper is a corpus file derived from another by a scripted edit.
type tamper struct {
	out  string
	from string
	edit func(name string, in []byte, rcp map[string]string) ([]byte, error)
}

const ageAll = "age: {srv-1},{srv-2},{operator-1}"

var encryptions = []encryption{
	// Every format, several recipients.
	{out: "values.sops.yaml", plain: "values.yaml", rule: ageAll},
	{out: "values.sops.json", plain: "values.json", rule: "age: {srv-1},{srv-2}"},
	{out: "values.sops.env", plain: "values.env", rule: "age: {srv-1}"},
	{out: "values.sops.ini", plain: "values.ini", rule: "age: {srv-1}"},
	{out: "blob.sops.bin", plain: "blob.bin", rule: "age: {srv-1}"},
	{out: "tls.sops.pem", plain: "tls.pem", rule: "age: {srv-1}"},
	// Encrypted comments inside lists (list items tagged type:comment),
	// before, between and after real elements.
	{out: "list-comments.sops.yaml", plain: "list-comments.yaml", rule: "age: {srv-1}"},
	// The re-encrypted twin of values.sops.yaml: same values and recipients,
	// a new data key.
	{out: "values-reencrypted.sops.yaml", plain: "values.yaml", rule: ageAll},
	// Encryption-rule variants.
	{out: "unencrypted-suffix.sops.yaml", plain: "values.yaml", rule: "age: {srv-1}\n    unencrypted_suffix: _plain"},
	{out: "encrypted-regex.sops.yaml", plain: "values.yaml", rule: "age: {srv-1}\n    encrypted_regex: ^(password|value)$"},
	{out: "mac-only-encrypted.sops.yaml", plain: "values.yaml", rule: "age: {srv-1}\n    mac_only_encrypted: true"},
	// Shamir key groups: srv-1 alone is one group short of the threshold.
	{
		out: "key-groups.sops.yaml", plain: "values.yaml",
		rule: "shamir_threshold: 2\n    key_groups:\n" +
			"      - age: [{srv-1}, {operator-1}]\n" +
			"      - age: [{srv-2}]\n" +
			"      - age: [{srv-pq}]",
	},
	// Hybrid post-quantum next to X25519.
	{out: "pq.sops.yaml", plain: "values.yaml", rule: "age: {srv-pq},{srv-1}"},
	// Bases for scripted edits below (kept in the corpus, too).
	{out: "base-tamper.sops.yaml", plain: "values.yaml", rule: "age: {srv-1},{srv-2}"},
}

var tampers = []tamper{
	{out: "tampered-ciphertext.sops.yaml", from: "base-tamper.sops.yaml", edit: editCiphertext},
	{out: "tampered-mac.sops.yaml", from: "base-tamper.sops.yaml", edit: editPlainValue},
	{out: "tampered-type.sops.yaml", from: "base-tamper.sops.yaml", edit: editTypeTag},
	{out: "tampered-recipient.sops.yaml", from: "base-tamper.sops.yaml", edit: editRecipientLabel},
	{out: "no-mac.sops.yaml", from: "base-tamper.sops.yaml", edit: dropMAC},
	{out: "non-age.sops.yaml", from: "base-tamper.sops.yaml", edit: addNonAgeKeys},
}

func main() {
	dir := flag.String("dir", "testdata/sops", "corpus root (holds plain/, corpus/ and recipients.txt)")
	flag.Parse()
	if err := run(*dir); err != nil {
		log.Fatal(err)
	}
}

func run(dir string) error {
	rcp, err := readRecipients(filepath.Join(dir, "recipients.txt"))
	if err != nil {
		return err
	}
	outDir := filepath.Join(dir, "corpus")
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", outDir, err)
	}
	tmp, err := os.MkdirTemp("", "picolet-sops-corpus-")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	for _, e := range encryptions {
		if err := encrypt(e, dir, tmp, rcp); err != nil {
			return fmt.Errorf("%s: %w", e.out, err)
		}
	}
	for _, t := range tampers {
		in, err := os.ReadFile(filepath.Join(outDir, t.from))
		if err != nil {
			return fmt.Errorf("%s: %w", t.out, err)
		}
		out, err := t.edit(t.out, in, rcp)
		if err != nil {
			return fmt.Errorf("%s: %w", t.out, err)
		}
		if err := os.WriteFile(filepath.Join(outDir, t.out), out, 0o600); err != nil {
			return fmt.Errorf("%s: %w", t.out, err)
		}
	}
	return writePlaintextSOPS(outDir)
}

// writePlaintextSOPS writes the negative case: a *.sops.* file that was never
// encrypted.
func writePlaintextSOPS(outDir string) error {
	plain := "# TEST-ONLY: a *.sops.* file left in plaintext (never encrypted).\ndb:\n    password: hunter2-test\n"
	return os.WriteFile(filepath.Join(outDir, "plaintext.sops.yaml"), []byte(plain), 0o600)
}

func readRecipients(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading recipients: %w", err)
	}
	defer func() { _ = f.Close() }()
	rcp := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, key, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("recipients: malformed line %q", line)
		}
		rcp[name] = strings.TrimSpace(key)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading recipients: %w", err)
	}
	return rcp, nil
}

func substitute(s string, rcp map[string]string) (string, error) {
	for name, key := range rcp {
		s = strings.ReplaceAll(s, "{"+name+"}", key)
	}
	if strings.Contains(s, "{") {
		return "", fmt.Errorf("unknown recipient placeholder in %q", s)
	}
	return s, nil
}

func encrypt(e encryption, dir, tmp string, rcp map[string]string) error {
	rule, err := substitute(e.rule, rcp)
	if err != nil {
		return err
	}
	// An explicit config keeps sops from picking up a .sops.yaml above the
	// working directory.
	cfg := filepath.Join(tmp, "sops-"+e.out+".yaml")
	cfgBody := "creation_rules:\n  - path_regex: \"\"\n    " + rule + "\n"
	if err := os.WriteFile(cfg, []byte(cfgBody), 0o600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	plain, err := filepath.Abs(filepath.Join(dir, "plain", e.plain))
	if err != nil {
		return fmt.Errorf("resolving plain path: %w", err)
	}
	out, err := filepath.Abs(filepath.Join(dir, "corpus", e.out))
	if err != nil {
		return fmt.Errorf("resolving output path: %w", err)
	}
	cmd := exec.Command("go", "tool", "sops", "--config", cfg, "encrypt", "--output", out, plain)
	cmd.Env = sopsFreeEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sops encrypt: %w: %s", err, stderr.String())
	}
	return nil
}

// sopsFreeEnv drops every SOPS_* variable, so the operator's environment
// (key services, extra recipients) never leaks into the corpus.
func sopsFreeEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SOPS_") {
			env = append(env, kv)
		}
	}
	return env
}

// replaceOnce replaces old with repl and fails unless old occurs exactly once,
// so a change in sops' output format breaks the generator instead of silently
// producing an untampered file.
func replaceOnce(name string, in []byte, old, repl string) ([]byte, error) {
	if n := bytes.Count(in, []byte(old)); n != 1 {
		return nil, fmt.Errorf("%s: expected exactly one %q, found %d", name, old, n)
	}
	return bytes.Replace(in, []byte(old), []byte(repl), 1), nil
}

// lineWithPrefix returns the single line that starts with prefix, indentation
// included.
func lineWithPrefix(name string, in []byte, prefix string) (string, error) {
	var found []string
	for line := range strings.SplitSeq(string(in), "\n") {
		if strings.HasPrefix(line, prefix) {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("%s: expected exactly one line starting with %q, found %d", name, prefix, len(found))
	}
	return found[0], nil
}

// dbPasswordPrefix starts the line of db.password (hosts.*.password is
// indented deeper).
const dbPasswordPrefix = "    password: ENC[AES256_GCM,data:"

// editCiphertext flips one character of db.password's ciphertext.
func editCiphertext(name string, in []byte, _ map[string]string) ([]byte, error) {
	line, err := lineWithPrefix(name, in, dbPasswordPrefix)
	if err != nil {
		return nil, err
	}
	i := strings.Index(line, "data:") + len("data:")
	c := byte('A')
	if line[i] == 'A' {
		c = 'B'
	}
	return replaceOnce(name, in, line, line[:i]+string(c)+line[i+1:])
}

// editPlainValue changes a value sops left in plaintext; it is still covered
// by the MAC, so decryption fails with a MAC mismatch.
func editPlainValue(name string, in []byte, _ map[string]string) ([]byte, error) {
	return replaceOnce(name, in, "note_unencrypted: visible", "note_unencrypted: edited")
}

// editTypeTag relabels db.password's type tag from str to int.
func editTypeTag(name string, in []byte, _ map[string]string) ([]byte, error) {
	line, err := lineWithPrefix(name, in, dbPasswordPrefix)
	if err != nil {
		return nil, err
	}
	return replaceOnce(name, in, line, strings.Replace(line, ",type:str]", ",type:int]", 1))
}

// editRecipientLabel relabels srv-2's stanza as srv-3. The label is not
// authenticated: srv-1 still decrypts, srv-3 passes the recipient pre-check
// but its decrypt fails, and srv-2 is no longer named, so it is not a
// recipient.
func editRecipientLabel(name string, in []byte, rcp map[string]string) ([]byte, error) {
	return replaceOnce(name, in, "recipient: "+rcp["srv-2"], "recipient: "+rcp["srv-3"])
}

// dropMAC removes sops.mac, which makes the file not a SOPS file for
// `sops filestatus` and picolet.
func dropMAC(name string, in []byte, _ map[string]string) ([]byte, error) {
	line, err := lineWithPrefix(name, in, "    mac: ENC[")
	if err != nil {
		return nil, err
	}
	return replaceOnce(name, in, line+"\n", "")
}

// nonAgeKeys is a KMS and a PGP master key in the same key group as the age
// stanzas. Their wrapped data keys are dummies: no KMS or PGP key can open
// them, so the file decrypts only through age, and a decryptor that tried them
// would fail (or reach out to AWS).
const nonAgeKeys = `    kms:
        - arn: arn:aws:kms:us-east-1:111122223333:key/00000000-0000-0000-0000-000000000000
          created_at: "2026-01-01T00:00:00Z"
          enc: VEVTVC1PTkxZIGR1bW15IEtNUyBzdGFuemE=
          aws_profile: ""
    pgp:
        - created_at: "2026-01-01T00:00:00Z"
          enc: |-
            -----BEGIN PGP MESSAGE-----

            VEVTVC1PTkxZIGR1bW15IFBHUCBzdGFuemE=
            -----END PGP MESSAGE-----
          fp: 0000000000000000000000000000000000000000
`

// addNonAgeKeys inserts the dummy KMS and PGP keys right after `sops:`.
func addNonAgeKeys(name string, in []byte, _ map[string]string) ([]byte, error) {
	return replaceOnce(name, in, "\nsops:\n", "\nsops:\n"+nonAgeKeys)
}

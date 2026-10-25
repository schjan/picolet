package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// translate finds where the running container shows a path of the next
// start's mount: under a mount with the same source, unless shadowed.
func TestTranslate(t *testing.T) {
	t.Parallel()
	creds := mountSource{kind: sourceVolume, name: "/srv/creds"}
	next := mount{source: creds, target: "/new"}
	at, shadow := translate(next, "hook", []mount{{source: creds, target: "/old"}})
	assert.Equal(t, "/old/hook", at)
	assert.Nil(t, shadow)

	tmpfs := mount{target: "/old/hook"}
	at, shadow = translate(next, "hook", []mount{{source: creds, target: "/old"}, tmpfs})
	assert.Empty(t, at)
	assert.Equal(t, &tmpfs, shadow)

	at, _ = translate(next, "hook", []mount{{source: mountSource{kind: sourceVolume, name: "/srv/creds", subpath: "a"}, target: "/old"}})
	assert.Empty(t, at, "another subpath of the same volume shows other files")
	at, _ = translate(mount{target: "/new"}, "hook", []mount{{target: "/new"}})
	assert.Empty(t, at, "an anonymous volume is never shared")
}

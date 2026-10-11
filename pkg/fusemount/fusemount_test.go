package fusemount

import (
	"fmt"
	"io/fs"
	"syscall"
	"testing"
)

func TestClassify(t *testing.T) {
	const tmpfsMagic = 0x01021994
	tests := []struct {
		name   string
		fsType uint64
		err    error
		want   State
	}{
		{"fuse answers", fuseSuperMagic, nil, Live},
		{"other filesystem answers", tmpfsMagic, nil, Other},
		{"daemon gone", 0, syscall.ENOTCONN, Dead},
		{"daemon gone, wrapped", 0, fmt.Errorf("statfs: %w", syscall.ENOTCONN), Dead},
		{"no such path", 0, syscall.ENOENT, Gone},
		{"no such path, as the os reports it", 0, fs.ErrNotExist, Gone},
		{"permission denied says nothing about fuse", 0, syscall.EACCES, Other},
		{"io error says nothing about fuse", 0, syscall.EIO, Other},
		// The type of a failed statfs is zero, but a stale value must not make a
		// failure look live.
		{"fuse magic with an error", fuseSuperMagic, syscall.EIO, Other},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classify(tt.fsType, tt.err); got != tt.want {
				t.Errorf("classify(%#x, %v) = %v, want %v", tt.fsType, tt.err, got, tt.want)
			}
		})
	}
}

func TestStateString(t *testing.T) {
	for state, want := range map[State]string{Live: "live", Dead: "dead", Gone: "gone", Other: "other", State(42): "other"} {
		if got := state.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", int(state), got, want)
		}
	}
}

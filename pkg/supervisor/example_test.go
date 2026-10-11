package supervisor_test

import (
	"context"
	"errors"
	"log"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/fusemount"
	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/scope"
	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/state"
	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/supervisor"
)

type volume struct {
	ID         string
	MountPoint string
	Remote     string
}

func Example() {
	ctx := context.Background()

	// One JSON file per volume, written before its daemon starts.
	volumes := state.Store[volume]{
		Path: func(id string) string { return filepath.Join("/var/lib/example/volumes", id+".json") },
		Glob: "/var/lib/example/volumes/*.json",
	}

	scopes, err := scope.New(ctx, scope.Config{Prefix: "csi-example-", MemoryMax: 1 << 30})
	if err != nil {
		log.Fatal(err)
	}
	if err := scopes.FellBack(); err != nil {
		log.Printf("no host systemd, daemons die with this pod: %v", err)
	}

	start := func(ctx context.Context, sup *supervisor.Supervisor, v volume) error {
		cmd := exec.Command("rclone", "mount", v.Remote, v.MountPoint)
		_, err := sup.Start(ctx, v.ID, cmd, v.MountPoint, supervisor.StartOptions{Timeout: time.Minute})
		return err
	}

	var sup *supervisor.Supervisor
	sup, err = supervisor.New(supervisor.Config{
		Scope: scopes,
		IDs: func() ([]string, error) {
			all, errs := volumes.LoadAll()
			ids := make([]string, 0, len(all))
			for _, v := range all {
				ids = append(ids, v.ID)
			}
			return ids, errors.Join(errs...)
		},
		Reconcile: func(ctx context.Context, id string, first bool) {
			v, err := volumes.Load(id) // under the lock: the volume may be gone
			if err != nil {
				return
			}
			if first {
				log.Printf("volume %s was left by an earlier driver", id)
			}
			if fusemount.Status(v.MountPoint) == fusemount.Dead {
				_ = fusemount.Unmount(v.MountPoint)
				if err := start(ctx, sup, v); err != nil {
					log.Printf("remount %s: %v", id, err)
				}
			}
		},
		OnError: func(err error) { log.Print(err) },
	})
	if err != nil {
		log.Fatal(err)
	}
	go sup.Run(ctx)

	// In the CSI calls that change a volume:
	sup.Lock("vol-1")
	defer sup.Unlock("vol-1")
	// ... save the state, then start the daemon.
}

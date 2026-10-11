.PHONY: test
test:
	go test -race ./...

# Mount tests need root and a Linux kernel, so they run in a privileged
# container. On a non-Linux machine that container is inside colima or another VM.
# /dev/fuse is needed for the tests that mount a FUSE filesystem of their own.
MOUNTTEST = docker run --rm --privileged $(MOUNTTEST_DOCKER_FLAGS) -v $(CURDIR):/src -w /src \
	-e GOFLAGS=-buildvcs=false golang:1.27-alpine go test -tags mounttest

.PHONY: test-mount
test-mount:
	$(MOUNTTEST) -count=1 -v ./...

# The same tests with the host's systemd handed in, so the scope tests run against
# a real service manager. Needs a Docker host that runs systemd.
.PHONY: test-mount-systemd
test-mount-systemd:
	$(MAKE) test-mount MOUNTTEST_DOCKER_FLAGS="--pid=host -v /run/systemd:/run/systemd -e MOUNTTEST_SYSTEMD=1"

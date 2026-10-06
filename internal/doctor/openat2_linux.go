package doctor

import "golang.org/x/sys/unix"

func probeOpenat2(dir string) error {
	// RESOLVE_BENEATH rejects absolute paths (EXDEV): resolve "." beneath
	// a descriptor for the directory, the way the helper resolves
	// workspace paths beneath the data directory.
	dfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(dfd)
	fd, err := unix.Openat2(dfd, ".", &unix.OpenHow{
		Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS})
	if err == nil {
		unix.Close(fd)
	}
	return err
}

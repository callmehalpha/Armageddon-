package helper

import (
	"os/user"
	"strconv"
)

type sysUser struct {
	name     string
	uid, gid uint32
}

func lookupUser(name string) (sysUser, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return sysUser{}, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return sysUser{name: name, uid: uint32(uid), gid: uint32(gid)}, nil
}

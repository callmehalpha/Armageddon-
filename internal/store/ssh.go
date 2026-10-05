package store

// DeviceByPublicKey resolves a device by its base64 Ed25519 public key (the
// SSH endpoint authenticates device keys, plan M4.4).
func (s *Store) DeviceByPublicKey(pub string) (*Device, error) {
	return scanDevice(s.db.QueryRow(`SELECT `+deviceCols+` FROM devices WHERE public_key = ?`, pub))
}

// WorkspacesForMemberBySlug returns the user's workspaces with this slug
// (slugs are unique per owner, so a member may see more than one).
func (s *Store) WorkspacesForMemberBySlug(userID, slug string) ([]*Workspace, error) {
	all, err := s.WorkspacesForUser(userID)
	if err != nil {
		return nil, err
	}
	var out []*Workspace
	for _, w := range all {
		if w.Slug == slug {
			out = append(out, w)
		}
	}
	return out, nil
}

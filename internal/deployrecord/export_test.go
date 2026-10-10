package deployrecord

// SetTags makes newTag return tags in order, for tests that need to know
// which tag compaction keeps. It returns the restore.
func SetTags(tags ...string) func() {
	saved := newTag
	newTag = func() string {
		t := tags[0]
		tags = tags[1:]
		return t
	}
	return func() { newTag = saved }
}

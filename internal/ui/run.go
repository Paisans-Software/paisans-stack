package ui

// Run does work inside a step titled title, and ends the step by how work
// ended: done, or failed with its error, which Run returns for the caller to
// print. On a terminal the step's spinner, with its elapsed time, shows while
// work runs, so a read over ssh that prints nothing until it answers does not
// look like a hang.
//
// It is for work that reports nothing of its own, Eg: a plan built from what
// ssh reads on each host. Work that opens its own steps already shows them.
func Run(r Reporter, title string, work func() error) error {
	_, err := Get(r, title, func() (struct{}, error) { return struct{}{}, work() })
	return err
}

// Get is Run for work that returns a value.
func Get[T any](r Reporter, title string, work func() (T, error)) (T, error) {
	s := r.Step(title)
	v, err := work()
	if err != nil {
		s.Fail(err)
		return v, err
	}
	s.Done("")
	return v, nil
}

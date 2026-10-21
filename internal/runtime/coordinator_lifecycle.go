package runtime

// Done closes when the coordinator stops.
func (c *Coordinator) Done() <-chan struct{} { return c.done }

// Wait returns the terminal error after the coordinator stops.
func (c *Coordinator) Wait() error {
	<-c.done
	return c.terminalError()
}

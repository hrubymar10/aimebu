package supervisor

// keyOf is the key this client holds for an agent ("" when none), for the tests.
func (c *Client) keyOf(agentID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.agentKeys[agentID]
}

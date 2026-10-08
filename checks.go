package decisionnode

// The checks below run every local check the matching call runs before it
// sends, alone: the same rules, the limits held from the last Models call
// included, and the same error. They send nothing and upload nothing, and
// return nil for a request the call would send. Use them to validate input
// before spending time on it, for example before reading large images.

// CheckDecide checks a request as Decide (and DecideMany, for each request)
// would before sending it.
func (c *Client) CheckDecide(req Request) error {
	if c.err != nil {
		return c.err
	}
	_, err := c.prepare(req, nil)
	return err
}

// CheckSession checks an open request as OpenSession would before sending
// it.
func (c *Client) CheckSession(r SessionRequest) error {
	if c.err != nil {
		return c.err
	}
	_, err := c.sessionBody(r)
	return err
}

// CheckBatchCreate checks the requests of a batch as Batches().Create (and
// RunBatch) would: the custom_id rules of the call, then each line as a
// decide request. No requests is valid here: an empty draft.
func (c *Client) CheckBatchCreate(reqs []BatchRequest) error {
	if c.err != nil {
		return c.err
	}
	_, err := c.prepareBatch(reqs, true)
	return err
}

// CheckBatchAdd checks requests as Batches().Add would: as CheckBatchCreate,
// except that a call with no requests is refused. The batch's id plays no
// part in the check.
func (c *Client) CheckBatchAdd(reqs []BatchRequest) error {
	if c.err != nil {
		return c.err
	}
	_, err := c.prepareBatch(reqs, false)
	return err
}

// CheckFrame checks a frame as Send would before writing it: nothing is
// written, nothing is uploaded and no seq is used.
func (s *Session) CheckFrame(f Frame) error {
	if s.client == nil {
		return errSessionWithoutClient
	}
	_, _, err := s.checkFrame(f)
	return err
}

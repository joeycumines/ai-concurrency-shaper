package transcode

// BodyLimits holds the independent body limits of a transcoded route. The
// limits are deliberately separate: the accepted-request limit bounds inbound
// payloads, the retry-replay limit bounds what the retry transport may replay,
// and the buffered-response limit bounds non-streaming upstream JSON reads.
//
// https://platform.claude.com/docs/en/api/overview
type BodyLimits struct {
	AcceptedRequestBytes    int64
	DecodedRequestBytes     int64
	RetryReplayBytes        int64
	SuccessfulResponseBytes int64
	ErrorResponseBytes      int64
	SSELineBytes            int
	SSEFrameBytes           int
}

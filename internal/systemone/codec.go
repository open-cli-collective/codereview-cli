package systemone

// Encode returns the exact compact request bytes used by the System One
// client, together with their request plan.
func Encode(request Request, limits Limits) (Plan, []byte, error) {
	return prepare(request, limits)
}

// Response contains validated response metadata and choice answers. Model and
// Usage may be populated when answer validation fails, matching client receipt
// behavior.
type Response struct {
	Model   string
	Answers map[string]ChoiceAnswer
	Usage   *Usage
}

// Decode validates a bounded System One response against request and limits.
// It parses protocol bytes directly and does not represent an HTTP exchange.
func Decode(data []byte, request Request, limits Limits) (Response, error) {
	if _, _, err := prepare(request, limits); err != nil {
		return Response{}, err
	}
	if len(data) > maxResponseBytes {
		return Response{}, newError(errorResponseTooLarge, "systemone: response exceeds local limit")
	}
	answers, model, usage, err := parseResponse(data, request)
	return Response{Model: model, Answers: answers, Usage: usage}, err
}

package transport

const HealthChallenge = "netforge.health.v1"

const HealthResponse = "netforge.health.v1 ok"

func IsHealthChallenge(payload []byte) bool {
	return string(payload) == HealthChallenge
}

func IsHealthResponse(payload []byte) bool {
	return string(payload) == HealthResponse
}

func HealthAnswer(payload []byte) []byte {
	if IsHealthChallenge(payload) {
		return []byte(HealthResponse)
	}
	return append([]byte(nil), payload...)
}

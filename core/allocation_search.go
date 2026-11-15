package core

// allocationSearch holds repeated matched comparisons for one candidate.
// Endpoints and mixed routes are all members of the same allocation space.
type allocationSearch struct {
	preferred int
	pending   int
	wins      int
	gains     [3]float64
}

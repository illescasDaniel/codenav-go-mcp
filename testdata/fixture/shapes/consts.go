package shapes

// Sizes are defaults.
const (
	// Unit is the side.
	Unit = 1.0 // metres
	Half = 0.5
)

const (
	KindA = iota
	KindB
)

var Small, Big = 0.5, 10.0

var P, Q = pair()

func pair() (int, int) { return 1, 2 }

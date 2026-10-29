package shapes

// Shape is a port.
type Shape interface {
	Area() float64
}

type Square struct{ Side float64 }

func (s Square) Area() float64 { return s.Side * s.Side }

type base struct{}

func (base) Area() float64 { return 0 }

// Embedded gets Area through embedding.
type Embedded struct{ base }

func Scale(s Shape, k float64, unused int) float64 { return s.Area() * k }

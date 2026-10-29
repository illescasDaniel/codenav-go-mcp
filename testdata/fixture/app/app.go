package app

import "example.com/fx/shapes"

func Total(xs []shapes.Shape) float64 {
	t := 0.0
	for _, x := range xs {
		t += shapes.Scale(x, 2, 0)
	}
	return t
}

var _ shapes.Shape = shapes.Square{}
var _ shapes.Shape = shapes.Embedded{}

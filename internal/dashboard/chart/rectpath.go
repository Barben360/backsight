package chart

import "strings"

// rectPaths merges rectangles that share a class into one path each, in the
// order the classes first appear. On a long window a chart draws a rectangle
// a day, over a thousand elements that each repeat their tag, class and four
// attribute names; one path says the same in a third of the bytes. The marks
// carry no text of their own (the hover rects and the table do), so nothing
// is lost.
type rectPaths struct {
	order []string
	d     map[string]*strings.Builder
}

// add draws the rectangle x, y, w, h under class.
func (r *rectPaths) add(class string, x, y, w, h float64) {
	if r.d == nil {
		r.d = map[string]*strings.Builder{}
	}
	b, ok := r.d[class]
	if !ok {
		b = &strings.Builder{}
		r.d[class] = b
		r.order = append(r.order, class)
	}
	b.WriteString("M " + f(x) + " " + f(y) + "h" + f(w) + "v" + f(h) + "h-" + f(w) + "z")
}

// write puts the paths into b, each with its class.
func (r *rectPaths) write(b *strings.Builder) {
	for _, class := range r.order {
		b.WriteString(`<path class="` + class + `" d="` + r.d[class].String() + `"></path>`)
	}
}

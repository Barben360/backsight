package termstyle

// The logo's shades (decision 0084 D4, approved in
// https://github.com/Barben360/backsight/issues/258): the inner arc is
// Brand; the middle and outer arcs stand for the SVG's 0.6 and 0.3
// opacities (rosewood blended toward grey, readable on dark and light
// themes); the dot keeps the logo's orange. Numbered from 100 so text
// roles added later never collide with them.
const (
	ArcMiddle Role = iota + 100
	ArcOuter
	Dot
)

func init() {
	palette[ArcMiddle] = shade{rgb: []int{172, 99, 124}, idx: 132, basic: 35}
	palette[ArcOuter] = shade{rgb: []int{150, 114, 126}, idx: 138, basic: 35}
	palette[Dot] = shade{bold: true, rgb: []int{232, 145, 58}, idx: 208, basic: 33}
}

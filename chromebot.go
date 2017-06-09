package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"image/png"

	"os"

	"image/color"

	"github.com/fogleman/gg"
	cdp "github.com/neelance/cdp-go"
	"github.com/neelance/cdp-go/protocol/dom"
	"github.com/neelance/cdp-go/protocol/page"
)

type Box struct {
	X, Y, W, H int
}

type Step struct {
	Action   string   `json:"action"`
	Selector Selector `json:"selector"`
	Text     string   `json:"text"`
	URL      string   `json:"url"`
}

type Selector struct {
	Text       string `json:"text"`
	Background string `json:"background"`
}

type testRunner struct {
	steps       []*Step
	currentStep *Step
	cl          *cdp.Client
	doc         *dom.Node
	nodes       map[dom.NodeId]*dom.Node
	events      chan interface{}
	testLog     io.Writer
}

func main() {
	var test struct {
		Name  string  `json:"name"`
		Steps []*Step `json:"steps"`
	}

	if err := json.NewDecoder(os.Stdin).Decode(&test); err != nil {
		panic(err)
	}

	r := &testRunner{
		steps:       test.Steps,
		currentStep: &Step{},
	}

	var err error
	r.testLog, err = os.Create("log.html")
	if err != nil {
		panic(err)
	}

	fmt.Fprintf(r.testLog, `
		<link rel="stylesheet" href="https://maxcdn.bootstrapcdn.com/bootstrap/3.3.7/css/bootstrap.min.css" integrity="sha384-BVYiiSIFeK1dGmJRAkycuHAHRg32OmUcww7on3RYdg4Va+PmSTsz/K68vbdEjh4u" crossorigin="anonymous">
		<link rel="stylesheet" href="https://maxcdn.bootstrapcdn.com/bootstrap/3.3.7/css/bootstrap-theme.min.css" integrity="sha384-rHyoN1iRsVXV4nD0JutlnGaslCJuC7uwjduW9SVrLvRYooPp2bWYgmgJQIXwl/Sp" crossorigin="anonymous">

		<style>
			.panel-heading {
				white-space: nowrap;
				overflow: hidden;
				text-overflow: ellipsis;
			}
		</style>

		<div class="container">
			<h1>Test "%s"</h1>
	`, test.Name)

	closeAllTabs()
	r.cl = cdp.Dial(newTab())
	r.cl.Network.ClearBrowserCookies().Do()

	r.events = make(chan interface{}, 1024)
	r.cl.Events = r.events

	r.cl.Page.Enable().Do()
	r.cl.DOM.Enable().Do()
	r.cl.CSS.Enable().Do()

	var registerNodes func(n *dom.Node)
	registerNodes = func(n *dom.Node) {
		if _, ok := r.nodes[n.NodeId]; ok {
			panic("duplicate node")
		}
		r.nodes[n.NodeId] = n
		for _, c := range n.Children {
			registerNodes(c)
		}

		if n.ContentDocument != nil {
			registerNodes(n.ContentDocument)
			r.cl.DOM.RequestChildNodes().NodeId(n.ContentDocument.NodeId).Depth(-1).Do()
		}
	}

	r.consumeStep()

	for e := range r.events {
		log.Printf("%T\n", e)
		switch e := e.(type) {
		case *dom.DocumentUpdatedEvent:
			r.nodes = make(map[dom.NodeId]*dom.Node)

			result, err := r.cl.DOM.GetDocument().Do()
			if err != nil {
				panic(err)
			}
			r.doc = result.Root
			registerNodes(r.doc)

			r.cl.DOM.RequestChildNodes().NodeId(r.doc.NodeId).Depth(-1).Do()

		case *dom.SetChildNodesEvent:
			parent, ok := r.nodes[e.ParentId]
			if !ok {
				log.Printf("SetChildNodesEvent: node not found: %d", e.ParentId)
				break
			}
			parent.Children = e.Nodes
			for _, n := range e.Nodes {
				registerNodes(n)
				r.searchDOM(n)
			}

		case *dom.ChildNodeCountUpdatedEvent:
			n, ok := r.nodes[e.NodeId]
			if !ok {
				log.Printf("ChildNodeCountUpdatedEvent: node not found: %d", e.NodeId)
				break
			}
			n.ChildNodeCount = e.ChildNodeCount

		case *dom.ChildNodeInsertedEvent:
			parent, ok := r.nodes[e.ParentNodeId]
			if !ok {
				log.Printf("ChildNodeInsertedEvent: node not found: %d", e.ParentNodeId)
				break
			}
			parent.ChildNodeCount++

			i := 0
			if e.PreviousNodeId != 0 {
				i = childIndex(parent, e.PreviousNodeId) + 1
			}

			parent.Children = append(parent.Children, nil)
			copy(parent.Children[i+1:], parent.Children[i:])
			parent.Children[i] = e.Node

			registerNodes(e.Node)
			r.searchDOM(e.Node)

			r.cl.DOM.RequestChildNodes().NodeId(e.Node.NodeId).Depth(-1).Do()

		case *dom.ChildNodeRemovedEvent:
			parent, ok := r.nodes[e.ParentNodeId]
			if !ok {
				log.Printf("ChildNodeRemovedEvent: node not found: %d", e.ParentNodeId)
				break
			}
			parent.ChildNodeCount--

			i := childIndex(parent, e.NodeId)
			parent.Children = append(parent.Children[:i], parent.Children[i+1:]...)

		case *page.FrameNavigatedEvent:
			if e.Frame.URL == "about:blank" {
				break
			}
			if e.Frame.ParentId != "" {
				r.logPanel(fmt.Sprintf(`Frame navigated to <a href="%s">%s</a>`, e.Frame.URL, e.Frame.URL), "default")
				break
			}
			r.logPanel(fmt.Sprintf(`Navigated to <a href="%s">%s</a>`, e.Frame.URL, e.Frame.URL), "default")

		case *Step:
			json.NewEncoder(os.Stderr).Encode(e)
			switch e.Action {
			case "navigate":
				r.logPanel(fmt.Sprintf(`Navigate to <a href="%s">%s</a>`, e.URL, e.URL), "success")
				if _, err := r.cl.Page.Navigate().URL(e.URL).Do(); err != nil {
					panic(err)
				}
				r.consumeStep()

			case "find", "click":
				if r.doc != nil {
					r.searchDOM(r.doc)
				}

			case "type":
				for _, c := range e.Text {
					r.cl.Input.DispatchKeyEvent().Type("keyDown").Text(string(c)).Do()
					r.cl.Input.DispatchKeyEvent().Type("keyUp").Text(string(c)).Do()
				}
				r.logScreenshot(fmt.Sprintf("Type %q:", e.Text), "success", r.screenshot())
				r.consumeStep()

			case "printDOM":
				printDOM(r.doc, 0)
				r.consumeStep()
			}
		}
	}
}

func childIndex(n *dom.Node, childID dom.NodeId) int {
	for i, c := range n.Children {
		if c.NodeId == childID {
			return i
		}
	}
	panic("child not found")
}

func printDOM(n *dom.Node, indent int) {
	fmt.Printf("%s%s #%d\n", strings.Repeat("  ", indent), n.NodeName, n.NodeId)
	if n.ChildNodeCount != len(n.Children) {
		panic("children missing")
	}
	for _, c := range n.Children {
		printDOM(c, indent+1)
	}
	if n.ContentDocument != nil {
		printDOM(n.ContentDocument, indent+1)
	}
}

func (r *testRunner) consumeStep() {
	if len(r.steps) == 0 {
		os.Exit(0)
	}
	r.currentStep = r.steps[0]
	r.steps = r.steps[1:]
	r.events <- r.currentStep
}

const NodeTypeElement = 1
const NodeTypeAttribute = 2
const NodeTypeText = 3
const NodeTypeComment = 8

func (r *testRunner) searchDOM(n *dom.Node) {
	r.matchNode(n)

	for _, c := range n.Children {
		r.searchDOM(c)
	}
}

func (r *testRunner) matchNode(n *dom.Node) {
	switch r.currentStep.Action {
	case "find", "click":
		if !strings.Contains(visibleText(n), r.currentStep.Selector.Text) {
			return
		}

		id := n.NodeId
		if n.NodeType == NodeTypeText {
			id = n.ParentId
		}

		if r.currentStep.Selector.Background != "" {
			sc := r.getStyle(id, "background-color")
			c := parseCSSColor(sc)
			if c == nil {
				return
			}
			switch r.currentStep.Selector.Background {
			case "bright":
				y, _, _ := color.RGBToYCbCr(c.R, c.G, c.B)
				if y < 128 {
					return
				}
			case "dark":
				y, _, _ := color.RGBToYCbCr(c.R, c.G, c.B)
				if y >= 128 {
					return
				}
			default:
				panic("invalid color selector")
			}
		}

		box, err := r.getBoxModel(id)
		if err != nil {
			return
		}

		dc := gg.NewContextForImage(r.screenshot())
		addHighlight(dc, box)

		switch r.currentStep.Action {
		case "find":
			r.logScreenshot("Find element:", "success", dc.Image())
		case "click":
			r.logScreenshot("Click on element:", "success", dc.Image())
			x := box.X + (box.W / 2)
			y := box.Y + (box.H / 2)
			r.cl.Input.DispatchMouseEvent().Type("mousePressed").Button("left").X(x).Y(y).ClickCount(1).Do()
			r.cl.Input.DispatchMouseEvent().Type("mouseReleased").Button("left").X(x).Y(y).ClickCount(1).Do()
		}

		r.consumeStep()
	}
}

func visibleText(n *dom.Node) string {
	switch n.NodeType {
	case NodeTypeElement:
		if n.NodeName == "INPUT" {
			return getAttribute(n, "value")
		}
	case NodeTypeText:
		return n.NodeValue
	}
	return ""
}

func getAttribute(n *dom.Node, attrName string) string {
	for i := 0; i < len(n.Attributes); i += 2 {
		if n.Attributes[i] == attrName {
			return n.Attributes[i+1]
		}
	}
	return ""
}

func (r *testRunner) getStyle(nodeId dom.NodeId, styleName string) string {
	style, err := r.cl.CSS.GetComputedStyleForNode().NodeId(nodeId).Do()
	if err != nil {
		panic(err)
	}

	for _, s := range style.ComputedStyle {
		if s.Name == styleName {
			return s.Value
		}
	}
	return ""
}

func parseCSSColor(c string) *color.RGBA {
	if strings.HasPrefix(c, "rgb(") && strings.HasSuffix(c, ")") {
		if args := strings.Split(c[4:len(c)-1], ", "); len(args) == 3 {
			r, rErr := strconv.Atoi(args[0])
			g, gErr := strconv.Atoi(args[1])
			b, bErr := strconv.Atoi(args[2])
			if rErr == nil && gErr == nil && bErr == nil && r >= 0 && g >= 0 && b >= 0 && r < 256 && g < 256 && b < 256 {
				return &color.RGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 255}
			}
		}
	}

	if strings.HasPrefix(c, "rgba(") && strings.HasSuffix(c, ")") {
		if args := strings.Split(c[5:len(c)-1], ", "); len(args) == 4 {
			r, rErr := strconv.Atoi(args[0])
			g, gErr := strconv.Atoi(args[1])
			b, bErr := strconv.Atoi(args[2])
			a, aErr := strconv.Atoi(args[3])
			if rErr == nil && gErr == nil && bErr == nil && aErr != nil && r >= 0 && g >= 0 && b >= 0 && a >= 0 && r < 256 && g < 256 && b < 256 && a < 256 {
				return &color.RGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: uint8(a)}
			}
		}
	}

	return nil
}

func (r *testRunner) screenshot() image.Image {
	resp, err := r.cl.Page.CaptureScreenshot().Do()
	if err != nil {
		panic(err)
	}

	dec := base64.NewDecoder(base64.StdEncoding, strings.NewReader(resp.Data))
	img, err := png.Decode(dec)
	if err != nil {
		panic(err)
	}

	return img
}

func (r *testRunner) logScreenshot(title string, panelType string, img image.Image) {
	fmt.Fprintf(r.testLog, `
		<div class="panel panel-%s">
			<div class="panel-heading">%s</div>
			<div class="panel-body">
				<img src="data:image/png;base64,`, panelType, title)
	png.Encode(base64.NewEncoder(base64.StdEncoding, r.testLog), img)
	fmt.Fprintf(r.testLog, `">
			</div>
		</div>
	`)
}

func (r *testRunner) logPanel(title string, panelType string) {
	fmt.Fprintf(r.testLog, `
		<div class="panel panel-%s">
			<div class="panel-heading">%s</div>
		</div>
	`, panelType, title)
}

func (r *testRunner) getBoxModel(nodeId dom.NodeId) (*Box, error) {
	result, err := r.cl.DOM.GetBoxModel().NodeId(nodeId).Do()
	if err != nil {
		return nil, err
	}
	box := result.Model.Border
	x := int(box[0])
	y := int(box[1])
	w := int(box[4]) - x
	h := int(box[5]) - y
	return &Box{x, y, w, h}, nil
}

func addHighlight(dc *gg.Context, box *Box) {
	dc.SetRGB255(255, 0, 0)
	dc.SetLineWidth(2)
	dc.DrawRectangle(float64(box.X), float64(box.Y), float64(box.W), float64(box.H))
	dc.Stroke()
}

func newTab() string {
	resp, err := http.Get("http://localhost:9222/json/new")
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	var tab struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tab); err != nil {
		panic(err)
	}

	return tab.WebSocketDebuggerURL
}

func closeAllTabs() {
	resp, err := http.Get("http://localhost:9222/json")
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	var tabs []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tabs); err != nil {
		panic(err)
	}

	for _, tab := range tabs {
		closeTab(tab.ID)
	}
}

func closeTab(id string) {
	resp, err := http.Get("http://localhost:9222/json/close/" + id)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
}
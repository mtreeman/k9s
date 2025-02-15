package view

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/derailed/k9s/internal"
	"github.com/derailed/k9s/internal/client"
	"github.com/derailed/k9s/internal/config"
	"github.com/derailed/k9s/internal/dao"
	"github.com/derailed/k9s/internal/model"
	"github.com/derailed/k9s/internal/ui"
	"github.com/derailed/k9s/internal/ui/dialog"
	"github.com/derailed/k9s/internal/xray"
	"github.com/derailed/tview"
	"github.com/gdamore/tcell"
	"github.com/rs/zerolog/log"
	"github.com/sahilm/fuzzy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const xrayTitle = "Xray"

// Xray represents an xray tree view.
type Xray struct {
	*ui.Tree

	app      *App
	gvr      client.GVR
	meta     metav1.APIResource
	model    *model.Tree
	cancelFn context.CancelFunc
	envFn    EnvFunc
}

var _ ResourceViewer = (*Xray)(nil)

// NewXray returns a new view.
func NewXray(gvr client.GVR) ResourceViewer {
	return &Xray{
		gvr:   gvr,
		Tree:  ui.NewTree(),
		model: model.NewTree(gvr.String()),
	}
}

// Init initializes the view
func (x *Xray) Init(ctx context.Context) error {
	if err := x.Tree.Init(ctx); err != nil {
		return err
	}
	x.SetKeyListenerFn(x.keyEntered)

	var err error
	x.meta, err = dao.MetaFor(x.gvr)
	if err != nil {
		return err
	}

	if x.app, err = extractApp(ctx); err != nil {
		return err
	}

	x.bindKeys()
	x.SetBackgroundColor(config.AsColor(x.app.Styles.Xray().BgColor))
	x.SetBorderColor(config.AsColor(x.app.Styles.Xray().FgColor))
	x.SetBorderFocusColor(config.AsColor(x.app.Styles.Frame().Border.FocusColor))
	x.SetGraphicsColor(config.AsColor(x.app.Styles.Xray().GraphicColor))
	x.SetTitle(fmt.Sprintf(" %s-%s ", xrayTitle, strings.Title(x.gvr.R())))

	x.model.SetRefreshRate(time.Duration(x.app.Config.K9s.GetRefreshRate()) * time.Second)
	x.model.SetNamespace(client.CleanseNamespace(x.app.Config.ActiveNamespace()))
	x.model.AddListener(x)

	x.SetChangedFunc(func(n *tview.TreeNode) {
		ref, ok := n.GetReference().(xray.NodeSpec)
		if !ok {
			log.Error().Msgf("No ref found on node %s", n.GetText())
			return
		}
		x.SetSelectedItem(ref.Path)
		x.refreshActions()
	})
	x.refreshActions()

	return nil
}

// ExtraHints returns additional hints.
func (x *Xray) ExtraHints() map[string]string {
	if !x.app.Styles.Xray().ShowIcons {
		return nil
	}
	return xray.EmojiInfo()
}

// SetInstance sets specific resource instance.
func (x *Xray) SetInstance(string) {}

func (x *Xray) bindKeys() {
	x.Actions().Add(ui.KeyActions{
		tcell.KeyEnter:      ui.NewKeyAction("Goto", x.gotoCmd, true),
		ui.KeySlash:         ui.NewSharedKeyAction("Filter Mode", x.activateCmd, false),
		tcell.KeyBackspace2: ui.NewSharedKeyAction("Erase", x.eraseCmd, false),
		tcell.KeyBackspace:  ui.NewSharedKeyAction("Erase", x.eraseCmd, false),
		tcell.KeyDelete:     ui.NewSharedKeyAction("Erase", x.eraseCmd, false),
		tcell.KeyCtrlU:      ui.NewSharedKeyAction("Clear Filter", x.clearCmd, false),
		tcell.KeyEscape:     ui.NewSharedKeyAction("Filter Reset", x.resetCmd, false),
	})
}

func (x *Xray) keyEntered() {
	x.ClearSelection()
	x.update(x.filter(x.model.Peek()))
}

func (x *Xray) refreshActions() {
	aa := make(ui.KeyActions)

	defer func() {
		pluginActions(x, aa)
		hotKeyActions(x, aa)

		x.Actions().Add(aa)
		x.app.Menu().HydrateMenu(x.Hints())
	}()

	x.Actions().Clear()
	x.bindKeys()

	ref := x.selectedSpec()
	if ref == nil {
		return
	}

	var err error
	x.meta, err = dao.MetaFor(client.NewGVR(ref.GVR))
	if err != nil {
		log.Warn().Msgf("NO meta for %q -- %s", ref.GVR, err)
		return
	}

	if client.Can(x.meta.Verbs, "edit") {
		aa[ui.KeyE] = ui.NewKeyAction("Edit", x.editCmd, true)
	}
	if client.Can(x.meta.Verbs, "delete") {
		aa[tcell.KeyCtrlD] = ui.NewKeyAction("Delete", x.deleteCmd, true)
	}
	if !dao.IsK9sMeta(x.meta) {
		aa[ui.KeyY] = ui.NewKeyAction("YAML", x.viewCmd, true)
		aa[ui.KeyD] = ui.NewKeyAction("Describe", x.describeCmd, true)
	}

	if ref.GVR == "containers" {
		aa[ui.KeyS] = ui.NewKeyAction("Shell", x.shellCmd, true)
		aa[ui.KeyL] = ui.NewKeyAction("Logs", x.logsCmd(false), true)
		aa[ui.KeyShiftL] = ui.NewKeyAction("Logs Previous", x.logsCmd(true), true)
	}

	x.Actions().Add(aa)
}

// GetSelectedPath returns the current selection as string.
func (x *Xray) GetSelectedPath() string {
	ref := x.selectedSpec()
	if ref == nil {
		return ""
	}
	return ref.Path
}

func (x *Xray) selectedSpec() *xray.NodeSpec {
	node := x.GetCurrentNode()
	if node == nil {
		return nil
	}

	ref, ok := node.GetReference().(xray.NodeSpec)
	if !ok {
		log.Error().Msgf("Expecting a NodeSpec!")
		return nil
	}

	return &ref
}

// EnvFn returns an plugin env function if available.
func (x *Xray) EnvFn() EnvFunc {
	return x.envFn
}

// Aliases returns all available aliases.
func (x *Xray) Aliases() []string {
	return append(x.meta.ShortNames, x.meta.SingularName, x.meta.Name)
}

func (x *Xray) logsCmd(prev bool) func(evt *tcell.EventKey) *tcell.EventKey {
	return func(evt *tcell.EventKey) *tcell.EventKey {
		ref := x.selectedSpec()
		if ref == nil {
			return nil
		}

		if ref.Parent != nil {
			x.showLogs(ref.Parent, ref, prev)
		} else {
			log.Error().Msgf("No parent found for container %q", ref.Path)
		}

		return nil
	}
}

func (x *Xray) showLogs(pod, co *xray.NodeSpec, prev bool) {
	// Need to load and wait for pods
	ns, _ := client.Namespaced(pod.Path)
	_, err := x.app.factory.CanForResource(ns, "v1/pods", client.MonitorAccess)
	if err != nil {
		x.app.Flash().Err(err)
		return
	}

	if err := x.app.inject(NewLog(client.NewGVR(co.GVR), pod.Path, co.Path, prev)); err != nil {
		x.app.Flash().Err(err)
	}
}

func (x *Xray) shellCmd(evt *tcell.EventKey) *tcell.EventKey {
	ref := x.selectedSpec()
	if ref == nil {
		return nil
	}

	if ref.Status != "" {
		x.app.Flash().Errf("%s is not in a running state", ref.Path)
		return nil
	}

	if ref.Parent != nil {
		_, co := client.Namespaced(ref.Path)
		x.shellIn(ref.Parent.Path, co)
	} else {
		log.Error().Msgf("No parent found on container node %q", ref.Path)
	}

	return nil
}

func (x *Xray) shellIn(path, co string) {
	x.Stop()
	shellIn(x.app, path, co)
	x.Start()
}

func (x *Xray) viewCmd(evt *tcell.EventKey) *tcell.EventKey {
	ref := x.selectedSpec()
	if ref == nil {
		return evt
	}

	ctx := x.defaultContext()
	raw, err := x.model.ToYAML(ctx, ref.GVR, ref.Path)
	if err != nil {
		x.App().Flash().Errf("unable to get resource %q -- %s", ref.GVR, err)
		return nil
	}

	details := NewDetails(x.app, "YAML", ref.Path).Update(raw)
	if err := x.app.inject(details); err != nil {
		x.app.Flash().Err(err)
	}

	return nil

}

func (x *Xray) deleteCmd(evt *tcell.EventKey) *tcell.EventKey {
	ref := x.selectedSpec()
	if ref == nil {
		return evt
	}

	x.Stop()
	defer x.Start()
	{
		gvr := client.NewGVR(ref.GVR)
		meta, err := dao.MetaFor(gvr)
		if err != nil {
			log.Warn().Msgf("NO meta for %q -- %s", ref.GVR, err)
			return nil
		}
		x.resourceDelete(gvr, ref, fmt.Sprintf("Delete %s %s?", meta.SingularName, ref.Path))
	}

	return nil

}

func (x *Xray) describeCmd(evt *tcell.EventKey) *tcell.EventKey {
	ref := x.selectedSpec()
	if ref == nil {
		return evt
	}

	x.describe(ref.GVR, ref.Path)

	return nil
}

func (x *Xray) describe(gvr, path string) {
	ctx := context.Background()
	ctx = context.WithValue(ctx, internal.KeyFactory, x.app.factory)

	yaml, err := x.model.Describe(ctx, gvr, path)
	if err != nil {
		x.app.Flash().Errf("Describe command failed: %s", err)
		return
	}

	details := NewDetails(x.app, "Describe", path).Update(yaml)
	if err := x.app.inject(details); err != nil {
		x.app.Flash().Err(err)
	}
}

func (x *Xray) editCmd(evt *tcell.EventKey) *tcell.EventKey {
	ref := x.selectedSpec()
	if ref == nil {
		return evt
	}

	x.Stop()
	defer x.Start()
	{
		ns, n := client.Namespaced(ref.Path)
		args := make([]string, 0, 10)
		args = append(args, "edit")
		args = append(args, client.NewGVR(ref.GVR).R())
		args = append(args, "-n", ns)
		args = append(args, "--context", x.app.Config.K9s.CurrentContext)
		if cfg := x.app.Conn().Config().Flags().KubeConfig; cfg != nil && *cfg != "" {
			args = append(args, "--kubeconfig", *cfg)
		}
		if !runK(true, x.app, append(args, n)...) {
			x.app.Flash().Err(errors.New("Edit exec failed"))
		}
	}

	return evt
}

func (x *Xray) activateCmd(evt *tcell.EventKey) *tcell.EventKey {
	if x.app.InCmdMode() {
		return evt
	}
	x.app.Flash().Info("Filter mode activated.")
	x.CmdBuff().SetActive(true)

	return nil
}

func (x *Xray) clearCmd(evt *tcell.EventKey) *tcell.EventKey {
	if !x.CmdBuff().IsActive() {
		return evt
	}
	x.CmdBuff().Clear()
	x.model.ClearFilter()
	x.Start()

	return nil
}

func (x *Xray) eraseCmd(evt *tcell.EventKey) *tcell.EventKey {
	if x.CmdBuff().IsActive() {
		x.CmdBuff().Delete()
	}
	x.UpdateTitle()

	return nil
}

func (x *Xray) resetCmd(evt *tcell.EventKey) *tcell.EventKey {
	if !x.CmdBuff().InCmdMode() {
		x.CmdBuff().Reset()
		return x.app.PrevCmd(evt)
	}

	x.app.Flash().Info("Clearing filter...")
	x.CmdBuff().Reset()
	x.model.ClearFilter()
	x.Start()

	return nil
}

func (x *Xray) gotoCmd(evt *tcell.EventKey) *tcell.EventKey {
	if x.CmdBuff().IsActive() {
		if ui.IsLabelSelector(x.CmdBuff().String()) {
			x.Start()
		}
		x.CmdBuff().SetActive(false)
		x.GetRoot().ExpandAll()

		return nil
	}

	ref := x.selectedSpec()
	if ref == nil {
		return nil
	}
	if len(strings.Split(ref.Path, "/")) == 1 {
		return nil
	}
	if err := x.app.viewResource(client.NewGVR(ref.GVR).R(), ref.Path, false); err != nil {
		x.app.Flash().Err(err)
	}

	return nil
}

func (x *Xray) filter(root *xray.TreeNode) *xray.TreeNode {
	q := x.CmdBuff().String()
	if x.CmdBuff().Empty() || ui.IsLabelSelector(q) {
		return root
	}

	x.UpdateTitle()
	if ui.IsFuzzySelector(q) {
		return root.Filter(q, fuzzyFilter)
	}

	return root.Filter(q, rxFilter)
}

// TreeNodeSelected callback for node selection.
func (x *Xray) TreeNodeSelected() {
	x.app.QueueUpdateDraw(func() {
		n := x.GetCurrentNode()
		if n != nil {
			n.SetColor(config.AsColor(x.app.Styles.Xray().CursorColor))
		}
	})
}

// TreeLoadFailed notifies the load failed.
func (x *Xray) TreeLoadFailed(err error) {
	x.app.Flash().Err(err)
}

func (x *Xray) update(node *xray.TreeNode) {
	root := makeTreeNode(node, x.ExpandNodes(), x.app.Styles)
	if node == nil {
		x.app.QueueUpdateDraw(func() {
			x.SetRoot(root)
		})
		return
	}

	for _, c := range node.Children {
		x.hydrate(root, c)
	}
	if x.GetSelectedItem() == "" {
		x.SetSelectedItem(node.ID)
	}

	x.app.QueueUpdateDraw(func() {
		x.SetRoot(root)
		root.Walk(func(node, parent *tview.TreeNode) bool {
			ref, ok := node.GetReference().(xray.NodeSpec)
			if !ok {
				log.Error().Msgf("Expecting a NodeSpec but got %T", node.GetReference())
				return false
			}
			// BOZO!! Figure this out expand/collapse but the root
			if parent != nil {
				node.SetExpanded(x.ExpandNodes())
			} else {
				node.SetExpanded(true)
			}

			if ref.Path == x.GetSelectedItem() {
				node.SetExpanded(true).SetSelectable(true)
				x.SetCurrentNode(node)
			}
			return true
		})
	})
}

// TreeChanged notifies the model data changed.
func (x *Xray) TreeChanged(node *xray.TreeNode) {
	x.Count = node.Count(x.gvr.String())
	x.update(x.filter(node))
	x.UpdateTitle()
}

func (x *Xray) hydrate(parent *tview.TreeNode, n *xray.TreeNode) {
	node := makeTreeNode(n, x.ExpandNodes(), x.app.Styles)
	for _, c := range n.Children {
		x.hydrate(node, c)
	}
	parent.AddChild(node)
}

// SetEnvFn sets the custom environment function.
func (x *Xray) SetEnvFn(EnvFunc) {}

// Refresh refresh the view
func (x *Xray) Refresh() {
}

// BufferChanged indicates the buffer was changed.
func (x *Xray) BufferChanged(s string) {}

// BufferActive indicates the buff activity changed.
func (x *Xray) BufferActive(state bool, k ui.BufferKind) {
	x.app.BufferActive(state, k)
}

func (x *Xray) defaultContext() context.Context {
	ctx := context.WithValue(context.Background(), internal.KeyFactory, x.app.factory)
	ctx = context.WithValue(ctx, internal.KeyFields, "")
	if x.CmdBuff().Empty() {
		ctx = context.WithValue(ctx, internal.KeyLabels, "")
	} else {
		ctx = context.WithValue(ctx, internal.KeyLabels, ui.TrimLabelSelector(x.CmdBuff().String()))
	}

	return ctx
}

// Start initializes resource watch loop.
func (x *Xray) Start() {
	x.Stop()

	x.CmdBuff().AddListener(x.app.Cmd())
	x.CmdBuff().AddListener(x)

	ctx := x.defaultContext()
	ctx, x.cancelFn = context.WithCancel(ctx)
	x.model.Watch(ctx)
	x.UpdateTitle()
}

// Stop terminates watch loop.
func (x *Xray) Stop() {
	if x.cancelFn == nil {
		return
	}
	x.cancelFn()
	x.cancelFn = nil

	x.CmdBuff().RemoveListener(x.app.Cmd())
	x.CmdBuff().RemoveListener(x)
}

// SetBindKeysFn sets up extra key bindings.
func (x *Xray) SetBindKeysFn(BindKeysFunc) {}

// SetContextFn sets custom context.
func (x *Xray) SetContextFn(ContextFunc) {}

// Name returns the component name.
func (x *Xray) Name() string { return "XRay" }

// GetTable returns the underlying table.
func (x *Xray) GetTable() *Table { return nil }

// GVR returns a resource descriptor.
func (x *Xray) GVR() string { return x.gvr.String() }

// App returns the current app handle.
func (x *Xray) App() *App {
	return x.app
}

// UpdateTitle updates the view title.
func (x *Xray) UpdateTitle() {
	t := x.styleTitle()
	x.app.QueueUpdateDraw(func() {
		x.SetTitle(t)
	})
}

func (x *Xray) styleTitle() string {
	base := fmt.Sprintf("%s-%s", xrayTitle, strings.Title(x.gvr.R()))
	ns := x.model.GetNamespace()
	if client.IsAllNamespaces(ns) {
		ns = client.NamespaceAll
	}

	buff := x.CmdBuff().String()
	var title string
	if ns == client.ClusterScope {
		title = ui.SkinTitle(fmt.Sprintf(ui.TitleFmt, base, x.Count), x.app.Styles.Frame())
	} else {
		title = ui.SkinTitle(fmt.Sprintf(ui.NSTitleFmt, base, ns, x.Count), x.app.Styles.Frame())
	}
	if buff == "" {
		return title
	}

	if ui.IsLabelSelector(buff) {
		buff = ui.TrimLabelSelector(buff)
	}

	return title + ui.SkinTitle(fmt.Sprintf(ui.SearchFmt, buff), x.app.Styles.Frame())
}

func (x *Xray) resourceDelete(gvr client.GVR, ref *xray.NodeSpec, msg string) {
	dialog.ShowDelete(x.app.Content.Pages, msg, func(cascade, force bool) {
		x.app.Flash().Infof("Delete resource %s %s", ref.GVR, ref.Path)
		accessor, err := dao.AccessorFor(x.app.factory, gvr)
		if err != nil {
			log.Error().Err(err).Msgf("No accessor")
			return
		}

		nuker, ok := accessor.(dao.Nuker)
		if !ok {
			x.app.Flash().Errf("Invalid nuker %T", accessor)
			return
		}
		if err := nuker.Delete(ref.Path, true, true); err != nil {
			x.app.Flash().Errf("Delete failed with `%s", err)
		} else {
			x.app.Flash().Infof("%s `%s deleted successfully", x.GVR(), ref.Path)
			x.app.factory.DeleteForwarder(ref.Path)
		}
		x.Refresh()
	}, func() {})
}

// ----------------------------------------------------------------------------
// Helpers...

func fuzzyFilter(q, path string) bool {
	q = strings.TrimSpace(q[2:])
	mm := fuzzy.Find(q, []string{path})

	return len(mm) > 0
}

func rxFilter(q, path string) bool {
	rx := regexp.MustCompile(`(?i)` + q)
	tokens := strings.Split(path, xray.PathSeparator)
	for _, t := range tokens {
		if rx.MatchString(t) {
			return true
		}
	}

	return false
}

func makeTreeNode(node *xray.TreeNode, expanded bool, styles *config.Styles) *tview.TreeNode {
	n := tview.NewTreeNode("No data...")
	if node != nil {
		n.SetText(node.Title(styles.Xray()))
		spec := xray.NodeSpec{}
		if p := node.Parent; p != nil {
			spec.GVR, spec.Path = p.GVR, p.ID
		}
		n.SetReference(xray.NodeSpec{
			GVR:    node.GVR,
			Path:   node.ID,
			Parent: &spec,
		})
	}
	n.SetSelectable(true)
	n.SetExpanded(expanded)
	n.SetColor(config.AsColor(styles.Xray().CursorColor))
	n.SetSelectedFunc(func() {
		n.SetExpanded(!n.IsExpanded())
	})
	return n
}

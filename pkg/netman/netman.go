// Package netman provides a reusable Fyne widget for browsing and connecting to
// wireless networks via iwd. Embed a Networks widget in any window:
//
//	conn, _ := dbus.ConnectSystemBus()
//	nm, err := netman.New(conn, window)
//	window.SetContent(nm)
package netman

import (
	"embed"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/amenzhinsky/go-polkit"
	"github.com/godbus/dbus/v5"
	"github.com/joeflateau/go-iwd"
)

//go:embed img/wifi-strength*.svg
var signalSVGs embed.FS

//go:embed img/wifi-lock.svg
var lockSVG []byte

// networkAction is the polkit action we check before attempting a connection.
const networkAction = "org.freedesktop.NetworkManager.network-control"

// agentPath is the D-Bus object path we expose our credentials agent on so that
// iwd can call back to us when a network needs a passphrase.
const agentPath = dbus.ObjectPath("/net/connman/iwd/agent/fysh")

// Networks is a Fyne widget that lists nearby wireless networks and lets the
// user connect to them, prompting for a passphrase when one is required.
type Networks struct {
	widget.BaseWidget

	conn           *dbus.Conn
	handleError    func(error)
	handlePassword func(string) string

	ctl            *iwd.Iwd
	selectedDevice string

	mu   sync.Mutex // guards nets
	nets []*iwd.OrderedNetwork

	signalIcons []fyne.Resource
	lockIcon    fyne.Resource

	connected    *widget.Label
	netList      *widget.List
	refresh      *widget.Button
	deviceSelect *widget.Select
	content      fyne.CanvasObject
}

// New creates a Networks widget bound to the given system-bus connection. The
// handler functions deal with user input or error printing. It returns an
// error if the initial iwd state cannot be read.
func New(conn *dbus.Conn, handlePass func(string) string, handleErr func(error)) (*Networks, error) {
	ctl, err := iwd.New(conn)
	if err != nil {
		return nil, err
	}

	n := &Networks{conn: conn, handleError: handleErr, handlePassword: handlePass, ctl: ctl}
	n.ExtendBaseWidget(n)
	n.loadIcons()
	n.registerAgent()
	n.buildUI()

	if n.selectedDevice != "" {
		// Populate immediately from iwd's current knowledge so Menu works right
		// away, then trigger a fresh scan in the background. This happens here
		// (not only when the widget is shown) so the menu is usable standalone.
		n.refreshNets()
		go n.Scan()
	}
	return n, nil
}

// CreateRenderer implements fyne.Widget.
func (n *Networks) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(n.content)
}

// Menu returns the current networks as a fyne.Menu immediately and kicks off
// a background scan. When the scan completes, onUpdate is called with the new menu.
func (n *Networks) Menu(onUpdate func(*fyne.Menu)) *fyne.Menu {
	if onUpdate != nil {
		go func() {
			n.Scan()
			fyne.Do(func() {
				onUpdate(n.buildMenu())
			})
		}()
	}
	return n.buildMenu()
}

// loadIcons reads the embedded wifi-strength icons (weakest to strongest) and
// the lock icon, wrapping each as a themed resource so it tints to the theme.
func (n *Networks) loadIcons() {
	n.signalIcons = make([]fyne.Resource, 4)
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("img/wifi-strength-%d.svg", i+1)
		data, err := signalSVGs.ReadFile(name)
		if err != nil {
			continue
		}
		n.signalIcons[i] = theme.NewThemedResource(fyne.NewStaticResource(name, data))
	}
	n.lockIcon = theme.NewThemedResource(fyne.NewStaticResource("lock", lockSVG))
}

// registerAgent exports our credentials agent so iwd can request passphrases.
func (n *Networks) registerAgent() {
	ag := &iwdAgent{requestPassphrase: n.askPassphrase}
	if err := n.conn.Export(ag, agentPath, "net.connman.iwd.Agent"); err != nil {
		log.Println("export agent:", err)
	}
	iwdObj := n.conn.Object("net.connman.iwd", "/net/connman/iwd")
	if call := iwdObj.Call("net.connman.iwd.AgentManager.RegisterAgent", 0, agentPath); call.Err != nil {
		log.Println("register agent:", call.Err)
	}
}

// buildUI assembles the widget content and selects the primary adapter.
func (n *Networks) buildUI() {
	n.connected = widget.NewLabelWithStyle("(not connected)", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})

	// Network list: tap a row to connect to that network. The left icon doubles
	// as status - a checkmark on the connected network, otherwise a lock on
	// secured networks - followed by the SSID and the signal strength (right).
	n.netList = widget.NewList(func() int {
		return len(n.getNets())
	}, func() fyne.CanvasObject {
		return container.NewBorder(nil, nil,
			widget.NewIcon(nil),
			widget.NewIcon(nil),
			widget.NewLabel(""))
	}, func(id widget.ListItemID, o fyne.CanvasObject) {
		nets := n.getNets()
		if id >= len(nets) {
			return
		}
		c := o.(*fyne.Container)
		net := nets[id]
		c.Objects[0].(*widget.Label).SetText(net.Name)

		status := c.Objects[1].(*widget.Icon)
		switch {
		case net.Connected:
			status.SetResource(theme.ConfirmIcon())
		case net.Type != "open":
			status.SetResource(n.lockIcon)
		default:
			status.SetResource(nil)
		}

		c.Objects[2].(*widget.Icon).SetResource(n.signalIcons[signalLevel(net.SignalStrength)])
	})
	n.netList.OnSelected = n.onNetworkSelected

	n.refresh = widget.NewButtonWithIcon("", theme.ViewRefreshIcon(), func() { go n.Scan() })
	n.refresh.Disable()

	// devs lists the wireless interfaces (devices backed by a station); these are
	// what the adapter selector offers, with the first as the primary default.
	var devs []string
	for _, device := range n.ctl.Devices {
		for i := range n.ctl.Stations {
			if n.ctl.Stations[i].Path == device.Path {
				devs = append(devs, device.Name)
				break
			}
		}
	}

	n.deviceSelect = widget.NewSelect(devs, func(name string) {
		n.selectedDevice = name
		n.refresh.Enable()
		n.refreshNets()
		go n.Scan()
	})
	n.deviceSelect.PlaceHolder = "(no wireless adapters)"
	if len(devs) > 0 {
		n.selectedDevice = devs[0]
		n.deviceSelect.Selected = devs[0]
		n.refresh.Enable()
	}

	top := container.NewBorder(nil, nil, widget.NewIcon(theme.ComputerIcon()), n.refresh, n.deviceSelect)
	n.content = container.NewBorder(top, n.connected, nil, nil, n.netList)
}

// onNetworkSelected connects to the tapped network.
func (n *Networks) onNetworkSelected(id widget.ListItemID) {
	n.netList.Unselect(id)
	nets := n.getNets()
	if id >= len(nets) {
		return
	}
	n.connect(nets[id])
}

// buildMenu builds a menu from the currently known networks.
func (n *Networks) buildMenu() *fyne.Menu {
	nets := n.getNets()
	items := make([]*fyne.MenuItem, 0, len(nets))
	for _, net := range nets {
		net := net // capture per-iteration network for the closure
		item := fyne.NewMenuItem(net.Name, func() { n.connect(net) })
		item.Icon = n.signalIcons[signalLevel(net.SignalStrength)]
		item.Checked = net.Connected
		items = append(items, item)
	}
	return fyne.NewMenu("", items...)
}

// connect starts a connection to net in the background, prompting for a
// passphrase if required and reporting any failure through the error handler.
func (n *Networks) connect(net *iwd.OrderedNetwork) {
	// Connect blocks while iwd negotiates (and potentially gets auth).
	go func() {
		if !getPermission(networkAction) {
			n.handleError(fmt.Errorf("not authorized to manage network connections"))
			return
		}
		if err := net.Connect(n.conn); err != nil {
			n.handleError(connectError(err))
		}
		fyne.Do(n.refreshNets)
	}()
}

// refreshNets re-reads the current iwd state and updates the network list and
// the connected-network label.
func (n *Networks) refreshNets() {
	if newCtl, err := iwd.New(n.conn); err == nil {
		n.ctl = newCtl
	}
	var nets []*iwd.OrderedNetwork
	st := stationFor(n.ctl, n.selectedDevice)
	if st != nil {
		if ordered, err := st.GetOrderedNetworks(n.conn); err == nil {
			nets = ordered
		} else {
			log.Println("ordered networks err", err)
		}
	}

	addr := "(not connected)"
	if st != nil && st.ConnectedNetwork != nil {
		cp := *st.ConnectedNetwork
		for _, net := range nets {
			if net.Path == cp {
				addr = net.Name
			}
		}
	}

	n.mu.Lock()
	n.nets = nets
	n.mu.Unlock()

	fyne.Do(func() {
		// Refresh outside the lock: netList.Refresh re-enters via getNets.
		n.connected.SetText(addr)
		n.netList.Refresh()
	})
}

// getNets returns the current network list under lock. Callers iterate or index
// the returned slice (a stable header); refreshNets only ever replaces the slice
// wholesale, never mutates it in place, so the snapshot stays valid.
func (n *Networks) getNets() []*iwd.OrderedNetwork {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.nets
}

// Scan triggers a wireless scan on the selected adapter, blocks until it
// finishes (or times out after ~9s), then refreshes the network list.
// The Menu call also uses this refreshed cache.
func (n *Networks) Scan() error {
	st := stationFor(n.ctl, n.selectedDevice)
	if st == nil {
		return nil
	}
	err := st.Scan(n.conn)
	if err != nil {
		log.Println("scan err", err)
	}
	// Poll fresh state until the station reports it has stopped scanning.
	for i := 0; i < 30; i++ {
		lc, e := iwd.New(n.conn)
		if e != nil {
			break
		}
		cur := stationFor(lc, n.selectedDevice)
		if cur == nil || !cur.Scanning {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	n.refreshNets()
	return err
}

// askPassphrase ask the user for a password to the network specified.
func (n *Networks) askPassphrase(network dbus.ObjectPath) (string, *dbus.Error) {
	name := "this network"
	for _, net := range n.getNets() {
		if net.Path == network {
			name = net.Name
			break
		}
	}

	pass := n.handlePassword(name)
	if pass == "" {
		return "", dbus.MakeFailedError(fmt.Errorf("passphrase entry cancelled"))
	}
	return pass, nil
}

// connectError converts iwd's terse D-Bus errors into messages suitable for
// showing to the user.
func connectError(err error) error {
	var derr dbus.Error
	if errors.As(err, &derr) {
		switch derr.Name {
		case "net.connman.iwd.InvalidFormat":
			return errors.New("invalid password: a Wi-Fi password must be 8-63 characters")
		case "net.connman.iwd.Failed":
			return errors.New("could not connect - the password may be incorrect")
		case "net.connman.iwd.Aborted":
			return errors.New("connection attempt was cancelled")
		case "net.connman.iwd.NoAgent", "net.connman.iwd.NotSupported":
			return errors.New("this network type is not supported")
		}
	}
	return err
}

// getPermission asks polkit whether the current user may manage network
// connections, prompting them to authenticate if needed.
func getPermission(id string) bool {
	authority, err := polkit.NewAuthority()
	if err != nil {
		log.Println("polkit unavailable, skipping authorization check:", err)
		return true
	}
	defer authority.Close()

	res, err := authority.CheckAuthorization(id, nil,
		polkit.CheckAuthorizationAllowUserInteraction, "networks-connect")
	if err != nil {
		fyne.LogError("Failed to check authorization", err)
		return true
	}

	return res.IsAuthorized
}

// signalLevel maps an iwd signal strength (reported in units of 100 * dBm) onto
// an index into the wifi-strength icons: 0 (weakest) to 3 (strongest).
func signalLevel(strength int16) int {
	dbm := strength / 100
	switch {
	case dbm >= -55:
		return 3
	case dbm >= -67:
		return 2
	case dbm >= -78:
		return 1
	default:
		return 0
	}
}

// stationFor returns the iwd station backing the named device, falling back to
// the first available station (the primary adapter) when there is no exact match.
func stationFor(c *iwd.Iwd, name string) *iwd.Station {
	var devPath dbus.ObjectPath
	for _, d := range c.Devices {
		if d.Name == name {
			devPath = d.Path
			break
		}
	}
	for i := range c.Stations {
		if c.Stations[i].Path == devPath {
			return &c.Stations[i]
		}
	}
	if len(c.Stations) > 0 {
		return &c.Stations[0]
	}
	return nil
}

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
	nets           []*iwd.OrderedNetwork
	selectedDevice string

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
	return n, nil
}

// CreateRenderer implements fyne.Widget.
func (n *Networks) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(n.content)
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
		return len(n.nets)
	}, func() fyne.CanvasObject {
		return container.NewBorder(nil, nil,
			widget.NewIcon(nil),
			widget.NewIcon(nil),
			widget.NewLabel(""))
	}, func(id widget.ListItemID, o fyne.CanvasObject) {
		c := o.(*fyne.Container)
		net := n.nets[id]
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

	n.refresh = widget.NewButtonWithIcon("", theme.ViewRefreshIcon(), func() { go n.scan() })
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
		go n.scan()
	})
	n.deviceSelect.PlaceHolder = "(no wireless adapters)"
	if len(devs) > 0 {
		n.deviceSelect.SetSelected(devs[0]) // primary device by default
	}

	top := container.NewBorder(nil, nil, widget.NewIcon(theme.ComputerIcon()), n.refresh, n.deviceSelect)
	n.content = container.NewBorder(top, n.connected, nil, nil, n.netList)
}

// onNetworkSelected connects to the tapped network.
func (n *Networks) onNetworkSelected(id widget.ListItemID) {
	n.netList.Unselect(id)
	if id >= len(n.nets) {
		return
	}
	net := n.nets[id]

	// Connect blocks while iwd negotiates (and potentially gets auth).
	go func() {
		if !getPermission(networkAction) {
			n.handleError(fmt.Errorf("not authorized to manage network connections"))
			return
		}
		if err := net.Connect(n.conn); err != nil {
			n.handleError(connectError(err))
		}
		n.refreshNets()
	}()
}

// refreshNets re-reads the current iwd state and updates the network list and
// the connected-network label.
func (n *Networks) refreshNets() {
	if newCtl, err := iwd.New(n.conn); err == nil {
		n.ctl = newCtl
	}
	n.nets = nil
	st := stationFor(n.ctl, n.selectedDevice)
	if st != nil {
		if ordered, err := st.GetOrderedNetworks(n.conn); err == nil {
			n.nets = ordered
		} else {
			log.Println("ordered networks err", err)
		}
	}

	addr := "(not connected)"
	if st != nil && st.ConnectedNetwork != nil {
		cp := *st.ConnectedNetwork
		for _, net := range n.nets {
			if net.Path == cp {
				addr = net.Name
			}
		}
	}
	n.connected.SetText(addr)
	n.netList.Refresh()
}

// scan triggers a wireless scan on the selected adapter, waits for it to finish
// (with a timeout), then refreshes the network list.
func (n *Networks) scan() {
	st := stationFor(n.ctl, n.selectedDevice)
	if st == nil {
		return
	}
	if err := st.Scan(n.conn); err != nil {
		log.Println("scan err", err)
	}
	// Poll fresh state until the station reports it has stopped scanning.
	for i := 0; i < 30; i++ {
		lc, err := iwd.New(n.conn)
		if err != nil {
			break
		}
		cur := stationFor(lc, n.selectedDevice)
		if cur == nil || !cur.Scanning {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	n.refreshNets()
}

// askPassphrase ask the user for a password to the network specified.
func (n *Networks) askPassphrase(network dbus.ObjectPath) (string, *dbus.Error) {
	name := "this network"
	for _, net := range n.nets {
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

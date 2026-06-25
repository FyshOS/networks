package main

import (
	"embed"
	"fmt"
	"log"
	"os"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
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

// iwdAgent implements the net.connman.iwd.Agent interface. iwd invokes
// RequestPassphrase whenever a connection attempt needs credentials we have not
// already provided (i.e. a secured network that is not yet known).
type iwdAgent struct {
	requestPassphrase func(network dbus.ObjectPath) (string, *dbus.Error)
}

func (a *iwdAgent) RequestPassphrase(network dbus.ObjectPath) (string, *dbus.Error) {
	return a.requestPassphrase(network)
}

func (a *iwdAgent) Release() *dbus.Error { return nil }

func (a *iwdAgent) Cancel(reason string) *dbus.Error {
	log.Println("Passphrase request cancelled:", reason)
	return nil
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

func main() {
	conn, err := dbus.ConnectSystemBus()
	ctl, err := iwd.New(conn)
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}

	// devs lists the wireless interfaces (devices backed by a station); these are
	// what the adapter selector offers, with the first as the primary default.
	var devs []string
	for _, device := range ctl.Devices {
		for i := range ctl.Stations {
			if ctl.Stations[i].Path == device.Path {
				devs = append(devs, device.Name)
				break
			}
		}
	}

	a := app.New()
	w := a.NewWindow("Networks")

	// Load the embedded wifi-strength icons, ordered weakest to strongest. They
	// are wrapped as themed resources so they tint to match the current theme.
	signalIcons := make([]fyne.Resource, 4)
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("img/wifi-strength-%d.svg", i+1)
		data, err := signalSVGs.ReadFile(name)
		if err != nil {
			continue
		}
		signalIcons[i] = theme.NewThemedResource(fyne.NewStaticResource(name, data))
	}
	lockIcon := theme.NewThemedResource(fyne.NewStaticResource("lock", lockSVG))

	connected := widget.NewLabelWithStyle("(not connected)", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})

	// nets holds the networks currently shown in the network list, ordered by
	// signal strength. It is refreshed after every scan and connection attempt.
	var nets []*iwd.OrderedNetwork
	var selectedDevice string

	askPassphrase := func(network dbus.ObjectPath) (string, *dbus.Error) {
		name := "this network"
		for _, n := range nets {
			if n.Path == network {
				name = n.Name
				break
			}
		}

		result := make(chan string, 1)
		entry := widget.NewPasswordEntry()
		d := dialog.NewForm("Connect to "+name, "Connect", "Cancel",
			[]*widget.FormItem{widget.NewFormItem("Password", entry)},
			func(ok bool) {
				if ok {
					result <- entry.Text
				} else {
					result <- ""
				}
			}, w)
		d.Resize(fyne.NewSize(320, d.MinSize().Height))
		d.Show()

		pass := <-result
		if pass == "" {
			return "", dbus.MakeFailedError(fmt.Errorf("passphrase entry cancelled"))
		}
		return pass, nil
	}

	// Register our credentials agent with iwd so it can prompt for passphrases.
	ag := &iwdAgent{requestPassphrase: askPassphrase}
	if err := conn.Export(ag, agentPath, "net.connman.iwd.Agent"); err != nil {
		log.Println("export agent:", err)
	}
	iwdObj := conn.Object("net.connman.iwd", "/net/connman/iwd")
	if call := iwdObj.Call("net.connman.iwd.AgentManager.RegisterAgent", 0, agentPath); call.Err != nil {
		log.Println("register agent:", call.Err)
	}

	// Network list: tap a row to connect to that network. The left icon doubles
	// as status - a checkmark on the connected network, otherwise a lock on
	// secured networks - followed by the SSID and the signal strength (right).
	netList := widget.NewList(func() int {
		return len(nets)
	}, func() fyne.CanvasObject {
		return container.NewBorder(nil, nil,
			widget.NewIcon(nil),
			widget.NewIcon(nil),
			widget.NewLabel(""))
	}, func(id widget.ListItemID, o fyne.CanvasObject) {
		c := o.(*fyne.Container)
		n := nets[id]
		c.Objects[0].(*widget.Label).SetText(n.Name)

		status := c.Objects[1].(*widget.Icon)
		switch {
		case n.Connected:
			status.SetResource(theme.ConfirmIcon())
		case n.Type != "open":
			status.SetResource(lockIcon)
		default:
			status.SetResource(nil)
		}

		c.Objects[2].(*widget.Icon).SetResource(signalIcons[signalLevel(n.SignalStrength)])
	})

	refresh := widget.NewButtonWithIcon("", theme.ViewRefreshIcon(), nil)
	refresh.Disable()

	// refreshNets re-reads the current iwd state and updates the network list and
	// the connected-network label.
	refreshNets := func() {
		if newCtl, err := iwd.New(conn); err == nil {
			ctl = newCtl
		}
		nets = nil
		st := stationFor(ctl, selectedDevice)
		if st != nil {
			if ordered, err := st.GetOrderedNetworks(conn); err == nil {
				nets = ordered
			} else {
				log.Println("ordered networks err", err)
			}
		}

		addr := "(not connected)"
		if st != nil && st.ConnectedNetwork != nil {
			cp := *st.ConnectedNetwork
			for _, n := range nets {
				if n.Path == cp {
					addr = n.Name
				}
			}
		}
		connected.SetText(addr)
		netList.Refresh()
	}

	scan := func() {
		st := stationFor(ctl, selectedDevice)
		if st == nil {
			return
		}
		if err := st.Scan(conn); err != nil {
			log.Println("scan err", err)
		}
		// Poll fresh state until the station reports it has stopped scanning.
		for i := 0; i < 30; i++ {
			lc, err := iwd.New(conn)
			if err != nil {
				break
			}
			cur := stationFor(lc, selectedDevice)
			if cur == nil || !cur.Scanning {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		refreshNets()
	}

	netList.OnSelected = func(id widget.ListItemID) {
		netList.Unselect(id)
		if id >= len(nets) {
			return
		}
		n := nets[id]

		// Connect blocks while iwd negotiates (and potentially gets auth).
		go func() {
			if !getPermission(networkAction) {
				dialog.ShowError(fmt.Errorf("not authorized to manage network connections"), w)
				return
			}
			if err := n.Connect(conn); err != nil {
				dialog.ShowError(err, w)
			}
			refreshNets()
		}()
	}

	refresh.OnTapped = func() { go scan() }

	deviceSelect := widget.NewSelect(devs, func(name string) {
		selectedDevice = name
		refresh.Enable()
		refreshNets()
		go scan()
	})
	deviceSelect.PlaceHolder = "(no wireless adapters)"
	if len(devs) > 0 {
		deviceSelect.SetSelected(devs[0]) // primary device by default
	}

	top := container.NewBorder(nil, nil, widget.NewIcon(theme.ComputerIcon()), refresh, deviceSelect)
	w.SetContent(container.NewBorder(top, connected, nil, nil, netList))
	w.Resize(fyne.NewSize(320, 360))
	w.ShowAndRun()
}

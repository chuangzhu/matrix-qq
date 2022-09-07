package internal

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/duo/matrix-qq/internal/types"

	"github.com/Mrs4s/MiraiGo/client"
	"golang.org/x/image/draw"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridge/commands"
	"maunium.net/go/mautrix/bridge/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type WrappedCommandEvent struct {
	*commands.Event
	Bridge *QQBridge
	User   *User
	Portal *Portal
}

func (br *QQBridge) RegisterCommands() {
	proc := br.CommandProcessor.(*commands.Processor)
	proc.AddHandlers(
		cmdLogin,
		cmdLogout,
		cmdDeleteSession,
		cmdReconnect,
		cmdDisconnect,
		cmdPing,
		cmdDeletePortal,
		cmdDeleteAllPortals,
		cmdList,
		cmdSearch,
		cmdOpen,
		cmdSync,
	)
}

func wrapCommand(handler func(*WrappedCommandEvent)) func(*commands.Event) {
	return func(ce *commands.Event) {
		user := ce.User.(*User)
		var portal *Portal
		if ce.Portal != nil {
			portal = ce.Portal.(*Portal)
		}
		br := ce.Bridge.Child.(*QQBridge)
		handler(&WrappedCommandEvent{ce, br, user, portal})
	}
}

var (
	HelpSectionConnectionManagement = commands.HelpSection{Name: "Connection management", Order: 11}
	HelpSectionCreatingPortals      = commands.HelpSection{Name: "Creating portals", Order: 15}
	HelpSectionPortalManagement     = commands.HelpSection{Name: "Portal management", Order: 20}
	HelpSectionInvites              = commands.HelpSection{Name: "Group invites", Order: 25}
	HelpSectionMiscellaneous        = commands.HelpSection{Name: "Miscellaneous", Order: 30}
)

var cmdLogin = &commands.FullHandler{
	Func: wrapCommand(fnLogin),
	Name: "login",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAuth,
		Description: "Link the bridge to your QQ account.",
	},
}

func fnLogin(ce *WrappedCommandEvent) {
	if ce.User.Token != nil {
		if ce.User.IsLoggedIn() {
			ce.Reply("You're already logged in")
		} else {
			ce.Reply("You're already logged in. Perhaps you wanted to `reconnect`?")
		}
		return
	}

	qrChan, err := ce.User.Login()
	if err != nil {
		ce.User.log.Errorf("Failed to log in:", err)
		ce.Reply("Failed to log in: %v", err)
		return
	}

	var prevState client.QRCodeLoginState
	var qrEventID id.EventID
	for rsp := range qrChan {
		if prevState == rsp.State {
			continue
		}
		prevState = rsp.State

		switch rsp.State {
		case client.QRCodeCanceled:
			ce.Reply("QR code canceled. Please restart the login.")
		case client.QRCodeTimeout:
			ce.Reply("QR code timed out. Please restart the login.")
		case client.QRCodeWaitingForConfirm:
			ce.Reply("QR code scanned. Please confirm using your phone.")
		case client.QRCodeImageFetch:
			qrEventID = ce.User.sendQR(ce, rsp.ImageData, qrEventID)
		case 0:
			ce.Reply("Failed to log in.")
		}
	}
	_, _ = ce.Bot.RedactEvent(ce.RoomID, qrEventID)
}

func (user *User) sendQR(ce *WrappedCommandEvent, qrCode []byte, prevEvent id.EventID) id.EventID {
	url, ok := user.uploadQR(ce, qrCode)
	if !ok {
		return prevEvent
	}
	content := event.MessageEventContent{
		MsgType: event.MsgImage,
		Body:    "",
		URL:     url.CUString(),
	}
	if len(prevEvent) != 0 {
		content.SetEdit(prevEvent)
	}
	resp, err := ce.Bot.SendMessageEvent(ce.RoomID, event.EventMessage, &content)
	if err != nil {
		user.log.Errorln("Failed to send edited QR code to user:", err)
	} else if len(prevEvent) == 0 {
		prevEvent = resp.EventID
	}
	return prevEvent
}

func (user *User) uploadQR(ce *WrappedCommandEvent, qrCode []byte) (id.ContentURI, bool) {
	bot := user.bridge.AS.BotClient()

	src, _, err := image.Decode(bytes.NewReader(qrCode))
	if err != nil {
		user.log.Errorln("Failed to decode QR code image:", err)
		ce.Reply("Failed to decodeQR code image: %v", err)
		return id.ContentURI{}, false
	}
	dst := image.NewRGBA(image.Rect(0, 0, src.Bounds().Max.X*3, src.Bounds().Max.Y*3))
	draw.NearestNeighbor.Scale(dst, dst.Rect, src, src.Bounds(), draw.Over, nil)
	buf := new(bytes.Buffer)
	if err := png.Encode(buf, dst); err != nil {
		user.log.Errorln("Failed to scale up QR code image:", err)
		ce.Reply("Failed to scale up QR code image: %v", err)
	} else {
		qrCode = buf.Bytes()
	}

	resp, err := bot.UploadBytes(qrCode, "image/png")
	if err != nil {
		user.log.Errorln("Failed to upload QR code:", err)
		ce.Reply("Failed to upload QR code: %v", err)
		return id.ContentURI{}, false
	}
	return resp.ContentURI, true
}

var cmdLogout = &commands.FullHandler{
	Func: wrapCommand(fnLogout),
	Name: "logout",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAuth,
		Description: "Unlink the bridge from your QQ account.",
	},
}

func fnLogout(ce *WrappedCommandEvent) {
	if ce.User.Token == nil {
		ce.Reply("You're not logged in.")
		return
	} else if !ce.User.IsLoggedIn() {
		ce.Reply("You are not connected to QQ. Use the `reconnect` command to reconnect, or `delete-session` to forget all login information.")
		return
	}
	puppet := ce.Bridge.GetPuppetByUID(ce.User.UID)
	if puppet.CustomMXID != "" {
		err := puppet.SwitchCustomMXID("", "")
		if err != nil {
			ce.User.log.Warnln("Failed to logout-matrix while logging out of QQ:", err)
		}
	}
	ce.User.Client.Disconnect()
	ce.User.Client.Release()
	ce.User.Token = nil
	ce.User.removeFromUIDMap(status.BridgeState{StateEvent: status.StateLoggedOut})
	ce.User.DeleteConnection()
	ce.User.DeleteSession()
	ce.Reply("Logged out successfully.")
}

var cmdDeleteSession = &commands.FullHandler{
	Func: wrapCommand(fnDeleteSession),
	Name: "delete-session",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAuth,
		Description: "Delete session information and disconnect from QQ without sending a logout request.",
	},
}

func fnDeleteSession(ce *WrappedCommandEvent) {
	if ce.User.Token == nil && ce.User.Client == nil {
		ce.Reply("Nothing to purge: no session information stored and no active connection.")
		return
	}
	ce.User.removeFromUIDMap(status.BridgeState{StateEvent: status.StateLoggedOut})
	ce.User.DeleteConnection()
	ce.User.DeleteSession()
	ce.Reply("Session information purged")
}

var cmdReconnect = &commands.FullHandler{
	Func: wrapCommand(fnReconnect),
	Name: "reconnect",
	Help: commands.HelpMeta{
		Section:     HelpSectionConnectionManagement,
		Description: "Reconnect to QQ.",
	},
}

func fnReconnect(ce *WrappedCommandEvent) {
	if ce.User.Client == nil {
		if ce.User.Token == nil {
			ce.Reply("You're not logged into QQ. Please log in first.")
		} else {
			ce.User.Connect()
			ce.Reply("Started connecting to QQ")
		}
	} else {
		ce.User.DeleteConnection()
		ce.User.BridgeState.Send(status.BridgeState{StateEvent: status.StateTransientDisconnect, Error: QQNotConnected})
		ce.User.Connect()
		ce.Reply("Restarted connection to QQ")
	}
}

var cmdDisconnect = &commands.FullHandler{
	Func: wrapCommand(fnDisconnect),
	Name: "disconnect",
	Help: commands.HelpMeta{
		Section:     HelpSectionConnectionManagement,
		Description: "Disconnect from QQ (without logging out).",
	},
}

func fnDisconnect(ce *WrappedCommandEvent) {
	if ce.User.Client == nil {
		ce.Reply("You don't have a QQ connection.")
		return
	}
	ce.User.DeleteConnection()
	ce.Reply("Successfully disconnected. Use the `reconnect` command to reconnect.")
	ce.User.BridgeState.Send(status.BridgeState{StateEvent: status.StateBadCredentials, Error: QQNotConnected})
}

var cmdPing = &commands.FullHandler{
	Func: wrapCommand(fnPing),
	Name: "ping",
	Help: commands.HelpMeta{
		Section:     HelpSectionConnectionManagement,
		Description: "Check your connection to QQ.",
	},
}

func fnPing(ce *WrappedCommandEvent) {
	if ce.User.Token == nil {
		if ce.User.Client != nil {
			ce.Reply("Connected to QQ, but not logged in.")
		} else {
			ce.Reply("You're not logged into QQ.")
		}
	} else if ce.User.IsLoggedIn() {
		ce.Reply("Logged in as %s, connection to QQ OK (probably)", ce.User.UID.Uin)
	} else {
		ce.Reply("You're logged in as %s, but you don't have a QQ connection.", ce.User.UID.Uin)
	}
}

func canDeletePortal(portal *Portal, userID id.UserID) bool {
	if len(portal.MXID) == 0 {
		return false
	}

	members, err := portal.MainIntent().JoinedMembers(portal.MXID)
	if err != nil {
		portal.log.Errorfln("Failed to get joined members to check if portal can be deleted by %s: %v", userID, err)
		return false
	}
	for otherUser := range members.Joined {
		_, isPuppet := portal.bridge.ParsePuppetMXID(otherUser)
		if isPuppet || otherUser == portal.bridge.Bot.UserID || otherUser == userID {
			continue
		}
		user := portal.bridge.GetUserByMXID(otherUser)
		if user != nil && user.Token != nil {
			return false
		}
	}
	return true
}

var cmdDeletePortal = &commands.FullHandler{
	Func: wrapCommand(fnDeletePortal),
	Name: "delete-portal",
	Help: commands.HelpMeta{
		Section:     HelpSectionPortalManagement,
		Description: "Delete the current portal. If the portal is used by other people, this is limited to bridge admins.",
	},
	RequiresPortal: true,
}

func fnDeletePortal(ce *WrappedCommandEvent) {
	if !ce.User.Admin && !canDeletePortal(ce.Portal, ce.User.MXID) {
		ce.Reply("Only bridge admins can delete portals with other Matrix users")
		return
	}

	ce.Portal.log.Infoln(ce.User.MXID, "requested deletion of portal.")
	ce.Portal.Delete()
	ce.Portal.Cleanup(false)
}

var cmdDeleteAllPortals = &commands.FullHandler{
	Func: wrapCommand(fnDeleteAllPortals),
	Name: "delete-all-portals",
	Help: commands.HelpMeta{
		Section:     HelpSectionPortalManagement,
		Description: "Delete all portals.",
	},
}

func fnDeleteAllPortals(ce *WrappedCommandEvent) {
	portals := ce.Bridge.GetAllPortals()
	var portalsToDelete []*Portal

	if ce.User.Admin {
		portalsToDelete = portals
	} else {
		portalsToDelete = portals[:0]
		for _, portal := range portals {
			if canDeletePortal(portal, ce.User.MXID) {
				portalsToDelete = append(portalsToDelete, portal)
			}
		}
	}
	if len(portalsToDelete) == 0 {
		ce.Reply("Didn't find any portals to delete")
		return
	}

	leave := func(portal *Portal) {
		if len(portal.MXID) > 0 {
			_, _ = portal.MainIntent().KickUser(portal.MXID, &mautrix.ReqKickUser{
				Reason: "Deleting portal",
				UserID: ce.User.MXID,
			})
		}
	}
	customPuppet := ce.Bridge.GetPuppetByCustomMXID(ce.User.MXID)
	if customPuppet != nil && customPuppet.CustomIntent() != nil {
		intent := customPuppet.CustomIntent()
		leave = func(portal *Portal) {
			if len(portal.MXID) > 0 {
				_, _ = intent.LeaveRoom(portal.MXID)
				_, _ = intent.ForgetRoom(portal.MXID)
			}
		}
	}
	ce.Reply("Found %d portals, deleting...", len(portalsToDelete))
	for _, portal := range portalsToDelete {
		portal.Delete()
		leave(portal)
	}
	ce.Reply("Finished deleting portal info. Now cleaning up rooms in background.")

	go func() {
		for _, portal := range portalsToDelete {
			portal.Cleanup(false)
		}
		ce.Reply("Finished background cleanup of deleted portal rooms.")
	}()
}

func matchesQuery(str string, query string) bool {
	if query == "" {
		return true
	}
	return strings.Contains(strings.ToLower(str), query)
}

func formatContacts(bridge *QQBridge, input []*client.FriendInfo, query string) (result []string) {
	hasQuery := len(query) > 0
	for _, contact := range input {
		if len(contact.Nickname) == 0 {
			continue
		}
		uid := types.NewIntUserUID(contact.Uin)
		puppet := bridge.GetPuppetByUID(uid)

		if !hasQuery || matchesQuery(contact.Nickname, query) || matchesQuery(contact.Remark, query) || matchesQuery(uid.Uin, query) {
			result = append(result, fmt.Sprintf("* %s / [%s](https://matrix.to/#/%s) - `%s`", contact.Nickname, contact.Remark, puppet.MXID, uid.Uin))
		}
	}
	sort.Strings(result)
	return
}

func formatGroups(input []*client.GroupInfo, query string) (result []string) {
	hasQuery := len(query) > 0
	for _, group := range input {
		code := strconv.FormatInt(group.Code, 10)
		if !hasQuery || matchesQuery(group.Name, query) || matchesQuery(code, query) {
			result = append(result, fmt.Sprintf("* %s - `%s`", group.Name, code))
		}
	}
	sort.Strings(result)
	return
}

var cmdList = &commands.FullHandler{
	Func: wrapCommand(fnList),
	Name: "list",
	Help: commands.HelpMeta{
		Section:     HelpSectionMiscellaneous,
		Description: "Get a list of all contacts and groups.",
		Args:        "<`contacts`|`groups`> [_page_] [_items per page_]",
	},
	RequiresLogin: true,
}

func fnList(ce *WrappedCommandEvent) {
	if len(ce.Args) == 0 {
		ce.Reply("**Usage:** `list <contacts|groups> [page] [items per page]`")
		return
	}
	mode := strings.ToLower(ce.Args[0])
	if mode[0] != 'g' && mode[0] != 'c' {
		ce.Reply("**Usage:** `list <contacts|groups> [page] [items per page]`")
		return
	}
	var err error
	page := 1
	max := 100
	if len(ce.Args) > 1 {
		page, err = strconv.Atoi(ce.Args[1])
		if err != nil || page <= 0 {
			ce.Reply("\"%s\" isn't a valid page number", ce.Args[1])
			return
		}
	}
	if len(ce.Args) > 2 {
		max, err = strconv.Atoi(ce.Args[2])
		if err != nil || max <= 0 {
			ce.Reply("\"%s\" isn't a valid number of items per page", ce.Args[2])
			return
		} else if max > 400 {
			ce.Reply("Warning: a high number of items per page may fail to send a reply")
		}
	}

	contacts := mode[0] == 'c'
	typeName := "Groups"
	var result []string
	if contacts {
		typeName = "Contacts"
		result = formatContacts(ce.User.bridge, ce.User.Client.FriendList, "")
	} else {
		result = formatGroups(ce.User.Client.GroupList, "")
	}

	if len(result) == 0 {
		ce.Reply("No %s found", strings.ToLower(typeName))
		return
	}
	pages := int(math.Ceil(float64(len(result)) / float64(max)))
	if (page-1)*max >= len(result) {
		if pages == 1 {
			ce.Reply("There is only 1 page of %s", strings.ToLower(typeName))
		} else {
			ce.Reply("There are %d pages of %s", pages, strings.ToLower(typeName))
		}
		return
	}
	lastIndex := page * max
	if lastIndex > len(result) {
		lastIndex = len(result)
	}
	result = result[(page-1)*max : lastIndex]
	ce.Reply("### %s (page %d of %d)\n\n%s", typeName, page, pages, strings.Join(result, "\n"))
}

var cmdSearch = &commands.FullHandler{
	Func: wrapCommand(fnSearch),
	Name: "search",
	Help: commands.HelpMeta{
		Section:     HelpSectionMiscellaneous,
		Description: "Search for contacts or groups.",
		Args:        "<_query_>",
	},
	RequiresLogin: true,
}

func fnSearch(ce *WrappedCommandEvent) {
	if len(ce.Args) == 0 {
		ce.Reply("**Usage:** `search <query>`")
		return
	}

	query := strings.ToLower(strings.TrimSpace(strings.Join(ce.Args, " ")))
	formattedContacts := strings.Join(formatContacts(ce.User.bridge, ce.User.Client.FriendList, query), "\n")
	formattedGroups := strings.Join(formatGroups(ce.User.Client.GroupList, query), "\n")

	result := make([]string, 0, 2)
	if len(formattedContacts) > 0 {
		result = append(result, "### Contacts\n\n"+formattedContacts)
	}
	if len(formattedGroups) > 0 {
		result = append(result, "### Groups\n\n"+formattedGroups)
	}

	if len(result) == 0 {
		ce.Reply("No contacts or groups found")
		return
	}

	ce.Reply(strings.Join(result, "\n\n"))
}

var cmdOpen = &commands.FullHandler{
	Func: wrapCommand(fnOpen),
	Name: "open",
	Help: commands.HelpMeta{
		Section:     HelpSectionCreatingPortals,
		Description: "Open a group chat portal.",
		Args:        "<_group code_>",
	},
	RequiresLogin: true,
}

func fnOpen(ce *WrappedCommandEvent) {
	if len(ce.Args) == 0 {
		ce.Reply("**Usage:** `open <group code>`")
		return
	}

	code, err := strconv.ParseInt(ce.Args[0], 10, 64)
	if err != nil {
		ce.Reply("That does not look like a group code")
		return
	}
	info := ce.User.Client.FindGroup(code)
	if info == nil {
		ce.Reply("Failed to get group info: %v", err)
		return
	}
	m, err := ce.User.Client.GetGroupMembers(info)
	if err != nil {
		ce.Reply("Failed to get group members: %v", err)
		return
	}
	info.Members = m
	uid := types.NewGroupUID(ce.Args[0])
	ce.Log.Debugln("Importing", uid, "for", ce.User.MXID)
	portal := ce.User.GetPortalByUID(uid)
	if len(portal.MXID) > 0 {
		portal.UpdateMatrixRoom(ce.User, info, false)
		ce.Reply("Portal room synced.")
	} else {
		err = portal.CreateMatrixRoom(ce.User, info, true)
		if err != nil {
			ce.Reply("Failed to create room: %v", err)
		} else {
			ce.Reply("Portal room created.")
		}
	}
}

var cmdSync = &commands.FullHandler{
	Func: wrapCommand(fnSync),
	Name: "sync",
	Help: commands.HelpMeta{
		Section:     HelpSectionMiscellaneous,
		Description: "Synchronize data from QQ.",
		Args:        "<contacts/groups/space> [--create-portals]",
	},
	RequiresLogin: true,
}

func fnSync(ce *WrappedCommandEvent) {
	if len(ce.Args) == 0 {
		ce.Reply("**Usage:** `sync <contacts/groups/space> [--contact-avatars] [--create-portals]`")
		return
	}
	args := strings.ToLower(strings.Join(ce.Args, " "))
	contacts := strings.Contains(args, "contacts")
	appState := strings.Contains(args, "appstate")
	space := strings.Contains(args, "space")
	groups := strings.Contains(args, "groups") || space
	createPortals := strings.Contains(args, "--create-portals")
	contactAvatars := strings.Contains(args, "--contact-avatars")
	if contactAvatars && (!contacts || appState) {
		ce.Reply("`--contact-avatars` can only be used with `sync contacts`")
		return
	}

	if contacts {
		err := ce.User.ResyncContacts(contactAvatars)
		if err != nil {
			ce.Reply("Error resyncing contacts: %v", err)
		} else {
			ce.Reply("Resynced contacts")
		}
	}
	if space {
		if !ce.Bridge.Config.Bridge.PersonalFilteringSpaces {
			ce.Reply("Personal filtering spaces are not enabled on this instance of the bridge")
			return
		}
		keys := ce.Bridge.DB.Portal.FindPrivateChatsNotInSpace(ce.User.UID)
		count := 0
		for _, key := range keys {
			portal := ce.Bridge.GetPortalByUID(key)
			portal.addToSpace(ce.User)
			count++
		}
		plural := "s"
		if count == 1 {
			plural = ""
		}
		ce.Reply("Added %d DM room%s to space", count, plural)
	}
	if groups {
		err := ce.User.ResyncGroups(createPortals)
		if err != nil {
			ce.Reply("Error resyncing groups: %v", err)
		} else {
			ce.Reply("Resynced groups")
		}
	}
}

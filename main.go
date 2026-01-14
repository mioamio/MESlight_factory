package main

import (
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	fiberRecover "github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/jchv/go-webview2"
	"github.com/xuri/excelize/v2"
	_ "modernc.org/sqlite"
)

//go:embed index.html
var embedFS embed.FS

// --- СТРУКТУРЫ ---
type Order struct {
	ID            int    `json:"id"`
	Name          string `json:"name"`
	Type          string `json:"type"`
	TargetQty     int    `json:"target_qty"`
	AllowDups     int    `json:"allow_dups"`
	Status        string `json:"status"`
	CreatedAt     string `json:"created_at"`
	ManualOffset  int    `json:"manual_offset"`
	LocalScanned  int    `json:"local_scanned"`
	GlobalScanned int    `json:"global_scanned"`
	GlobalTarget  int    `json:"global_target"`
}
type ScanEntry struct {
	ID        int    `json:"id"`
	Serial    string `json:"serial"`
	OrderID   int    `json:"order_id"`
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	OrderName string `json:"order_name"`
}
type Stats struct {
	Total  int `json:"total"`
	C1     int `json:"c1"`
	C2     int `json:"c2"`
	Stapel int `json:"stapel"`
}
type DataPacket struct {
	Orders  []Order     `json:"orders"`
	History []ScanEntry `json:"history"`
	Stats   Stats       `json:"stats"`
	Online  int         `json:"online_count"`
}
type Break struct {
	Start string `json:"start"`
	End   string `json:"end"`
}
type Settings struct {
	ShiftStart string  `json:"shift_start"`
	ShiftEnd   string  `json:"shift_end"`
	Breaks     []Break `json:"breaks"`
}

var (
	globalStats Stats
	statsMu     sync.Mutex
)

// --- WEBSOCKET HUB ---
type Client struct {
	conn *websocket.Conn
	send chan []byte
}

type Hub struct {
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
	mu         sync.Mutex
}

var hub = Hub{
	broadcast:  make(chan []byte),
	register:   make(chan *Client),
	unregister: make(chan *Client),
	clients:    make(map[*Client]bool),
}

func (h *Hub) removeClient(client *Client) {
	if _, ok := h.clients[client]; ok {
		delete(h.clients, client)
		close(client.send)
	}
}

func (h *Hub) run() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("HUB RECOVERED: %v", r)
			go h.run()
		}
	}()

	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()

			go func(c *Client) {
				defer func() {
					if r := recover(); r != nil {
					}
				}()
				data := getDataPacket()
				msg := fiber.Map{"type": "update_data", "data": data}
				jsonMsg, _ := json.Marshal(msg)
				h.mu.Lock()
				if _, ok := h.clients[c]; ok {
					select {
					case c.send <- jsonMsg:
					default:
					}
				}
				h.mu.Unlock()
			}(client)

		case client := <-h.unregister:
			h.mu.Lock()
			h.removeClient(client)
			h.mu.Unlock()

		case message := <-h.broadcast:
			h.mu.Lock()
			for client := range h.clients {
				select {
				case client.send <- message:
				default:
					h.removeClient(client)
				}
			}
			h.mu.Unlock()
		}
	}
}

// --- ГЛОБАЛЬНЫЕ ---
var db *sql.DB

const DB_NAME = "production_data.db"
const PORT = "80"

func initDB() {
	var err error
	db, err = sql.Open("sqlite", DB_NAME)
	if err != nil {
		log.Fatal(err)
	}

	db.Exec("PRAGMA journal_mode=WAL;")
	db.Exec("PRAGMA synchronous=NORMAL;")
	db.Exec("PRAGMA busy_timeout = 5000;")

	query := `
	CREATE TABLE IF NOT EXISTS orders (
		id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, type TEXT NOT NULL, target_qty INTEGER DEFAULT 0, allow_dups INTEGER DEFAULT 0, status TEXT DEFAULT 'active', created_at TEXT, manual_offset INTEGER DEFAULT 0, completed_at TEXT, scanned_qty INTEGER DEFAULT 0
	);
	CREATE TABLE IF NOT EXISTS scans ( id INTEGER PRIMARY KEY AUTOINCREMENT, serial TEXT NOT NULL, order_id INTEGER, type TEXT, timestamp TEXT, FOREIGN KEY(order_id) REFERENCES orders(id) );
	CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT);
	CREATE INDEX IF NOT EXISTS idx_scans_timestamp ON scans(timestamp);
	CREATE INDEX IF NOT EXISTS idx_scans_order ON scans(order_id);
	CREATE INDEX IF NOT EXISTS idx_scans_check ON scans(order_id, serial);
	INSERT OR IGNORE INTO settings (key, value) VALUES ('shift_start', '08:00'); INSERT OR IGNORE INTO settings (key, value) VALUES ('shift_end', '20:00'); INSERT OR IGNORE INTO settings (key, value) VALUES ('breaks', '[]');
	`
	db.Exec(query)
	refreshGlobalStats()
}

func refreshGlobalStats() {
	todayStart := time.Now().Format("2006-01-02") + " 00:00:00"
	todayEnd := time.Now().Format("2006-01-02") + " 23:59:59"

	// 1. Считаем реальные сканирования за сегодня
	row := db.QueryRow(`SELECT COUNT(id), SUM(CASE WHEN type='conveyor_1' THEN 1 ELSE 0 END), SUM(CASE WHEN type='conveyor_2' THEN 1 ELSE 0 END), SUM(CASE WHEN type='stapel' THEN 1 ELSE 0 END) FROM scans WHERE timestamp >= ? AND timestamp <= ?`, todayStart, todayEnd)
	var t, c1, c2, st sql.NullInt64
	row.Scan(&t, &c1, &c2, &st)

	// 2. Добавляем ручные правки (manual_offset) от АКТИВНЫХ заказов
	// Это гарантирует, что если вы поправили цифру руками, она добавится в общий итог
	row2 := db.QueryRow(`SELECT SUM(manual_offset), SUM(CASE WHEN type='conveyor_1' THEN manual_offset ELSE 0 END), SUM(CASE WHEN type='conveyor_2' THEN manual_offset ELSE 0 END), SUM(CASE WHEN type='stapel' THEN manual_offset ELSE 0 END) FROM orders WHERE status = 'active'`)
	var mt, mc1, mc2, mst sql.NullInt64
	row2.Scan(&mt, &mc1, &mc2, &mst)

	statsMu.Lock()
	globalStats.Total = int(t.Int64 + mt.Int64)
	globalStats.C1 = int(c1.Int64 + mc1.Int64)
	globalStats.C2 = int(c2.Int64 + mc2.Int64)
	globalStats.Stapel = int(st.Int64 + mst.Int64)
	statsMu.Unlock()
}

func getCachedStats() Stats {
	statsMu.Lock()
	defer statsMu.Unlock()
	return globalStats
}

func getDataPacket() DataPacket {
	var p DataPacket
	rows, _ := db.Query(`SELECT id, name, type, target_qty, allow_dups, created_at, manual_offset, (scanned_qty + manual_offset) as local_scanned FROM orders WHERE status = 'active' ORDER BY id DESC`)
	defer rows.Close()
	for rows.Next() {
		var o Order
		rows.Scan(&o.ID, &o.Name, &o.Type, &o.TargetQty, &o.AllowDups, &o.CreatedAt, &o.ManualOffset, &o.LocalScanned)
		o.GlobalScanned = o.LocalScanned
		o.GlobalTarget = o.TargetQty
		p.Orders = append(p.Orders, o)
	}
	hRows, _ := db.Query(`SELECT s.id, s.serial, s.type, s.timestamp, o.name, o.id FROM scans s LEFT JOIN orders o ON s.order_id = o.id ORDER BY s.id DESC LIMIT 50`)
	defer hRows.Close()
	for hRows.Next() {
		var s ScanEntry
		var oid sql.NullInt64
		var oname sql.NullString
		hRows.Scan(&s.ID, &s.Serial, &s.Type, &s.Timestamp, &oname, &oid)
		s.OrderID = int(oid.Int64)
		s.OrderName = oname.String
		p.History = append(p.History, s)
	}
	p.Stats = getCachedStats()
	hub.mu.Lock()
	p.Online = len(hub.clients)
	hub.mu.Unlock()
	return p
}

func getSettings() Settings {
	s := Settings{Breaks: []Break{}}
	rows, _ := db.Query("SELECT key, value FROM settings")
	defer rows.Close()
	for rows.Next() {
		var k, v string
		rows.Scan(&k, &v)
		switch k {
		case "shift_start":
			s.ShiftStart = v
		case "shift_end":
			s.ShiftEnd = v
		case "breaks":
			json.Unmarshal([]byte(v), &s.Breaks)
		}
	}
	// Дефолтные значения, если база пустая
	if s.ShiftStart == "" {
		s.ShiftStart = "08:00"
	}
	if s.ShiftEnd == "" {
		s.ShiftEnd = "20:00"
	}
	return s
}

func broadcastUpdate() {
	packet := getDataPacket()
	msg := fiber.Map{"type": "update_data", "data": packet}
	jsonBytes, _ := json.Marshal(msg)
	go func() { hub.broadcast <- jsonBytes }()
}

func main() {
	runtime.LockOSThread()
	initDB()
	go hub.run()

	app := fiber.New(fiber.Config{DisableStartupMessage: true, BodyLimit: 10 * 1024 * 1024, JSONEncoder: json.Marshal, JSONDecoder: json.Unmarshal})
	app.Use(fiberRecover.New())
	app.Use(cors.New())

	realIP := getLocalIP()
	serverURL := fmt.Sprintf("http://%s", realIP)
	if PORT != "80" {
		serverURL += ":" + PORT
	}

	app.Use("/", func(c *fiber.Ctx) error {
		if c.Path() != "/" && c.Path() != "/index.html" {
			return c.Next()
		}
		fileData, _ := embedFS.ReadFile("index.html")
		html := string(fileData)
		html = strings.Replace(html, "{{ server_url }}", serverURL, -1)
		injectJS := fmt.Sprintf(`<script>setTimeout(()=>{const ipEls=document.querySelectorAll('.ip-display');ipEls.forEach(el=>el.innerText="IP: %s");},500);</script></body>`, realIP)
		html = strings.Replace(html, "</body>", injectJS, 1)
		c.Set("Content-Type", "text/html")
		return c.SendString(html)
	})

	app.Use("/ws", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			c.Locals("allowed", true)
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})

	app.Get("/ws", websocket.New(func(c *websocket.Conn) {
		client := &Client{conn: c, send: make(chan []byte, 256)}
		hub.register <- client
		go func() {
			defer c.Close()
			for {
				message, ok := <-client.send
				if !ok {
					c.WriteMessage(websocket.CloseMessage, []byte{})
					return
				}
				if err := c.WriteMessage(websocket.TextMessage, message); err != nil {
					return
				}
			}
		}()
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				hub.unregister <- client
				break
			}
		}
	}))

	app.Post("/action/add_order", func(c *fiber.Ctx) error {
		type Req struct {
			Name      string      `json:"name"`
			Type      string      `json:"type"`
			Target    interface{} `json:"target"` // interface чтобы принять строку или число
			AllowDups bool        `json:"allow_dups"`
		}
		var r Req
		if err := c.BodyParser(&r); err != nil {
			return c.SendStatus(400)
		}

		// Конвертация target в int
		target := 0
		switch v := r.Target.(type) {
		case float64:
			target = int(v)
		case string:
			target, _ = strconv.Atoi(v)
		}

		allow := 0
		if r.AllowDups {
			allow = 1
		}

		// ИЗМЕНЕНИЕ: Получаем ID созданной записи
		res, err := db.Exec("INSERT INTO orders (name, type, target_qty, allow_dups, created_at) VALUES (?, ?, ?, ?, ?)", strings.ToUpper(strings.TrimSpace(r.Name)), r.Type, target, allow, time.Now().Format("2006-01-02 15:04:05"))

		var newID int64 = 0
		if err == nil {
			newID, _ = res.LastInsertId()
			broadcastUpdate()
		}

		// Возвращаем ID клиенту
		return c.JSON(fiber.Map{"status": "ok", "id": newID})
	})

	app.Post("/action/edit_order", func(c *fiber.Ctx) error {
		type Req struct {
			ID        interface{} `json:"id"`
			Name      string      `json:"name"`
			Target    interface{} `json:"target"`
			Scanned   interface{} `json:"scanned_count"`
			AllowDups bool        `json:"allow_dups"`
		}
		var r Req
		if err := c.BodyParser(&r); err != nil {
			return c.SendStatus(400)
		}

		// Вспомогательная функция для приведения типов
		toInt := func(v interface{}) int {
			switch val := v.(type) {
			case float64:
				return int(val)
			case string:
				i, _ := strconv.Atoi(val)
				return i
			default:
				return 0
			}
		}

		id := toInt(r.ID)
		target := toInt(r.Target)

		// Конвертируем галочку в число (0 или 1) для базы
		dups := 0
		if r.AllowDups {
			dups = 1
		}

		// Обновляем данные заказа (Имя, План, Дубликаты)
		db.Exec("UPDATE orders SET name=?, target_qty=?, allow_dups=? WHERE id=?", strings.ToUpper(r.Name), target, dups, id)

		// Если передали новое количество факта (коррекция)
		if r.Scanned != nil {
			newTotal := toInt(r.Scanned)
			var phys int
			// Узнаем сколько было реально насканировано
			if err := db.QueryRow("SELECT scanned_qty FROM orders WHERE id=?", id).Scan(&phys); err == nil {
				// Разницу записываем в manual_offset
				db.Exec("UPDATE orders SET manual_offset=? WHERE id=?", newTotal-phys, id)
			}
		}

		// Пересчитываем статистику
		refreshGlobalStats()
		broadcastUpdate()

		return c.JSON(fiber.Map{"status": "ok"})
	})

	app.Post("/action/delete_order", func(c *fiber.Ctx) error {
		type Req struct {
			ID int `json:"id"`
		}
		var r Req
		c.BodyParser(&r)
		db.Exec("DELETE FROM scans WHERE order_id=?", r.ID)
		db.Exec("DELETE FROM orders WHERE id=?", r.ID)
		go func() { refreshGlobalStats(); broadcastUpdate() }()
		return c.JSON(fiber.Map{"status": "ok"})
	})

	app.Post("/action/scan", func(c *fiber.Ctx) error {
		type Req struct {
			Serial    string `json:"serial"`
			OrderID   int    `json:"order_id"`
			Timestamp string `json:"timestamp"` // Добавили поле
		}
		var r Req
		// Если JSON кривой или данных нет
		if err := c.BodyParser(&r); err != nil || r.Serial == "" || r.OrderID == 0 {
			return c.JSON(fiber.Map{"status": "error", "msg": "Ошибка данных"})
		}

		var oType, name string
		var allowDups, target, manual int

		// 1. Узнаем Имя заказа по ID
		if err := db.QueryRow("SELECT type, allow_dups, target_qty, manual_offset, name FROM orders WHERE id=?", r.OrderID).Scan(&oType, &allowDups, &target, &manual, &name); err != nil {
			return c.JSON(fiber.Map{"status": "error", "msg": "Заказ закрыт"})
		}

		// Проверка дубликатов (если дубли запрещены)
		if allowDups == 0 {
			var exists int
			db.QueryRow("SELECT 1 FROM scans WHERE order_id=? AND serial=? LIMIT 1", r.OrderID, r.Serial).Scan(&exists)
			if exists == 1 {
				return c.JSON(fiber.Map{"status": "duplicate", "msg": "Дубликат"})
			}
		}

		// ВРЕМЯ: Если клиент прислал timestamp (оффлайн скан), используем его. Иначе берем текущее.
		ts := r.Timestamp
		if ts == "" {
			ts = time.Now().Format("2006-01-02 15:04:05")
		}

		res, err := db.Exec("INSERT INTO scans (serial, order_id, type, timestamp) VALUES (?, ?, ?, ?)", r.Serial, r.OrderID, oType, ts)
		if err != nil {
			log.Println("SQL Error:", err)
			return c.JSON(fiber.Map{"status": "error", "msg": "Ошибка базы данных"})
		}

		sid, _ := res.LastInsertId()

		// Обновляем конкретный заказ
		db.Exec("UPDATE orders SET scanned_qty = scanned_qty + 1 WHERE id=?", r.OrderID)

		var sharedTotal int
		db.QueryRow("SELECT SUM(scanned_qty + manual_offset) FROM orders WHERE name = ? AND status = 'active'", name).Scan(&sharedTotal)

		entry := ScanEntry{ID: int(sid), Serial: r.Serial, Type: oType, Timestamp: ts, OrderName: name, OrderID: r.OrderID}

		statsMu.Lock()
		globalStats.Total++
		switch oType {
		case "conveyor_1":
			globalStats.C1++
		case "conveyor_2":
			globalStats.C2++
		case "stapel":
			globalStats.Stapel++
		}
		currentStatsSnapshot := globalStats
		statsMu.Unlock()

		go func() {
			msg := fiber.Map{
				"type": "scan_event",
				"data": fiber.Map{
					"order_id":    r.OrderID,
					"order_name":  name,
					"order_total": sharedTotal,
					"scan_entry":  entry,
					"stats":       currentStatsSnapshot,
				},
			}
			jsonBytes, _ := json.Marshal(msg)
			hub.broadcast <- jsonBytes
		}()
		return c.JSON(fiber.Map{"status": "success"})
	})

	app.Post("/action/delete_scan", func(c *fiber.Ctx) error {
		type Req struct {
			ID int `json:"id"`
		}
		var r Req
		if err := c.BodyParser(&r); err != nil {
			return c.JSON(fiber.Map{"status": "error", "msg": "Bad request"})
		}

		// Сначала узнаем ID заказа
		var oid int
		err := db.QueryRow("SELECT order_id FROM scans WHERE id=?", r.ID).Scan(&oid)

		if err == nil && oid > 0 {
			// Удаляем скан
			db.Exec("DELETE FROM scans WHERE id=?", r.ID)
			// Уменьшаем счетчик заказа (не даем уйти ниже нуля)
			db.Exec("UPDATE orders SET scanned_qty = MAX(0, scanned_qty - 1) WHERE id=?", oid)

			// Обновляем статистику для всех
			go func() {
				refreshGlobalStats()
				broadcastUpdate()
			}()
			return c.JSON(fiber.Map{"status": "ok"})
		}
		return c.JSON(fiber.Map{"status": "error", "msg": "Scan not found"})
	})

	app.Get("/action/get_settings", func(c *fiber.Ctx) error { return c.JSON(getSettings()) })
	app.Post("/action/save_settings", func(c *fiber.Ctx) error {
		var s Settings
		c.BodyParser(&s)
		db.Exec("INSERT OR REPLACE INTO settings (key, value) VALUES ('shift_start', ?)", s.ShiftStart)
		db.Exec("INSERT OR REPLACE INTO settings (key, value) VALUES ('shift_end', ?)", s.ShiftEnd)
		bJson, _ := json.Marshal(s.Breaks)
		db.Exec("INSERT OR REPLACE INTO settings (key, value) VALUES ('breaks', ?)", string(bJson))
		return c.JSON(fiber.Map{"status": "ok"})
	})

	app.Get("/api/analytics_data", func(c *fiber.Ctx) error {
		dFrom := c.Query("from", time.Now().Format("2006-01-02"))
		dTo := c.Query("to", time.Now().Format("2006-01-02"))
		oid := c.Query("order_id")
		where := "timestamp >= ? AND timestamp <= ?"
		args := []interface{}{dFrom + " 00:00:00", dTo + " 23:59:59"}
		if oid != "" {
			where += " AND order_id = ?"
			args = append(args, oid)
		}
		var total int
		db.QueryRow("SELECT COUNT(id) FROM scans WHERE "+where, args...).Scan(&total)
		var chartLabels []string
		var chartValues []int
		chartType := "day"
		if dFrom == dTo {
			chartType = "hour"
			rows, _ := db.Query("SELECT strftime('%H', timestamp) as h, COUNT(id) FROM scans WHERE "+where+" GROUP BY h", args...)
			defer rows.Close()
			data := make(map[int]int)
			for rows.Next() {
				var h, val int
				rows.Scan(&h, &val)
				data[h] = val
			}
			for i := 0; i < 24; i++ {
				chartLabels = append(chartLabels, fmt.Sprintf("%02d:00", i))
				chartValues = append(chartValues, data[i])
			}
		} else {
			rows, _ := db.Query("SELECT strftime('%Y-%m-%d', timestamp) as d, COUNT(id) FROM scans WHERE "+where+" GROUP BY d ORDER BY d", args...)
			defer rows.Close()
			data := make(map[string]int)
			for rows.Next() {
				var d string
				var val int
				rows.Scan(&d, &val)
				data[d] = val
			}
			tFrom, _ := time.Parse("2006-01-02", dFrom)
			tTo, _ := time.Parse("2006-01-02", dTo)
			for t := tFrom; !t.After(tTo); t = t.AddDate(0, 0, 1) {
				ds := t.Format("2006-01-02")
				chartLabels = append(chartLabels, t.Format("02.01"))
				chartValues = append(chartValues, data[ds])
			}
		}
		speed := 0
		if total > 0 {
			var minT, maxT string
			db.QueryRow("SELECT MIN(timestamp), MAX(timestamp) FROM scans WHERE "+where, args...).Scan(&minT, &maxT)
			if minT != "" {
				t1, _ := time.Parse("2006-01-02 15:04:05", minT)
				t2, _ := time.Parse("2006-01-02 15:04:05", maxT)
				hrs := t2.Sub(t1).Hours()
				if hrs < 1 {
					hrs = 1
				}
				speed = int(float64(total) / hrs)
			}
		}
		return c.JSON(fiber.Map{"total": total, "speed": speed, "chart_labels": chartLabels, "chart_values": chartValues, "chart_type": chartType})
	})

	// --- ИСПРАВЛЕННЫЙ ПРОГНОЗ (FORECAST) ---
	app.Get("/api/forecast_order", func(c *fiber.Ctx) error {
		oid := c.Query("order_id")
		if oid == "" {
			return c.JSON(fiber.Map{"error": "No order"})
		}

		st := getSettings()

		// Защита от кривого графика работы (если начало > конца, или равны)
		// Если график кривой, переходим в режим 24/7 (start=00:00, end=23:59)
		tStart, _ := time.Parse("15:04", st.ShiftStart)
		tEnd, _ := time.Parse("15:04", st.ShiftEnd)

		force24h := false
		if tStart.After(tEnd) || tStart.Equal(tEnd) {
			force24h = true
			// log.Println("Shift settings invalid, using 24/7 mode")
		}

		var target, scanned, manual int
		if err := db.QueryRow("SELECT target_qty, scanned_qty, manual_offset FROM orders WHERE id=?", oid).Scan(&target, &scanned, &manual); err != nil {
			return c.JSON(fiber.Map{"error": "Error"})
		}

		rem := target - (scanned + manual)
		if rem <= 0 {
			return c.JSON(fiber.Map{"status": "done"})
		}

		// Считаем скорость за последние 2 часа
		var recent int
		db.QueryRow("SELECT COUNT(id) FROM scans WHERE order_id=? AND timestamp >= ?", oid, time.Now().Add(-2*time.Hour).Format("2006-01-02 15:04:05")).Scan(&recent)
		speed := float64(recent) / 2.0
		if speed < 1 {
			speed = 1
		} // Минимальная скорость

		sim := time.Now()
		needed := float64(rem) / speed // Столько ЧАСОВ работы нужно

		limit := 0
		maxLimit := 500000 // Лимит ~1 год (в минутах)

		for needed > 0 && limit < maxLimit {
			limit++
			sim = sim.Add(time.Minute)

			// Проверяем, рабочее ли сейчас время
			working := false

			if force24h {
				working = true
			} else {
				cm := sim.Hour()*60 + sim.Minute()
				sm := tStart.Hour()*60 + tStart.Minute()
				em := tEnd.Hour()*60 + tEnd.Minute()

				if cm >= sm && cm < em {
					working = true
					// Проверка перерывов
					for _, b := range st.Breaks {
						bs, _ := time.Parse("15:04", b.Start)
						be, _ := time.Parse("15:04", b.End)
						if cm >= (bs.Hour()*60+bs.Minute()) && cm < (be.Hour()*60+be.Minute()) {
							working = false
							break
						}
					}
				}
			}

			if working {
				needed -= (1.0 / 60.0) // Вычитаем 1 минуту работы
			}
		}

		delta := time.Until(sim)
		d := int(delta.Hours()) / 24
		h := int(delta.Hours()) % 24
		hDelta := fmt.Sprintf("Через %dч", int(delta.Hours()))
		if d > 0 {
			hDelta = fmt.Sprintf("Через %dд %dч", d, h)
		}

		return c.JSON(fiber.Map{
			"status":          "ok",
			"completion_date": sim.Format("02.01 15:04"),
			"human_delta":     hDelta,
			"speed":           int(speed),
			"remaining":       rem,
		})
	})

	// НОВОЕ API: Получение истории конкретного заказа
	app.Get("/api/order_history", func(c *fiber.Ctx) error {
		oid := c.Query("id")
		if oid == "" {
			return c.JSON([]ScanEntry{})
		}

		// Берем последние 1000 записей по этому заказу
		rows, _ := db.Query(`SELECT s.id, s.serial, s.type, s.timestamp, o.name, o.id FROM scans s LEFT JOIN orders o ON s.order_id = o.id WHERE s.order_id = ? ORDER BY s.id DESC LIMIT 1000`, oid)
		defer rows.Close()

		var history []ScanEntry
		for rows.Next() {
			var s ScanEntry
			var oid sql.NullInt64
			var oname sql.NullString
			rows.Scan(&s.ID, &s.Serial, &s.Type, &s.Timestamp, &oname, &oid)
			s.OrderID = int(oid.Int64)
			s.OrderName = oname.String
			history = append(history, s)
		}
		// Если записей нет, возвращаем пустой массив, а не null
		if history == nil {
			history = []ScanEntry{}
		}
		return c.JSON(history)
	})

	app.Get("/export", func(c *fiber.Ctx) error {
		dFrom := c.Query("date_from")
		dTo := c.Query("date_to")
		cat := c.Query("category")

		filter := ""
		filePrefix := "All"
		switch cat {
		case "stapel":
			filter = "AND s.type='stapel'"
			filePrefix = "Stapel"
		case "line1_2":
			filter = "AND s.type IN ('conveyor_1', 'conveyor_2')"
			filePrefix = "Linii"
		}

		dateStr := time.Now().Format("02.01.2006")
		fileName := fmt.Sprintf("%s_%s.xlsx", filePrefix, dateStr)

		// 1. Детальный список сканирований
		q := fmt.Sprintf(`SELECT o.name, s.serial, s.timestamp, s.type FROM scans s LEFT JOIN orders o ON s.order_id=o.id WHERE s.timestamp >= '%s 00:00:00' AND s.timestamp <= '%s 23:59:59' %s ORDER BY s.timestamp DESC`, dFrom, dTo, filter)
		rows, _ := db.Query(q)
		defer rows.Close()

		f := excelize.NewFile()
		s := "Sheet1"
		idx := 2

		// Стили
		styleHeader, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true, Color: "#FFFFFF"}, Fill: excelize.Fill{Type: "pattern", Color: []string{"#3b82f6"}, Pattern: 1}, Alignment: &excelize.Alignment{Horizontal: "center"}})
		styleBorder, _ := f.NewStyle(&excelize.Style{Border: []excelize.Border{{Type: "left", Color: "000000", Style: 1}, {Type: "top", Color: "000000", Style: 1}, {Type: "bottom", Color: "000000", Style: 1}, {Type: "right", Color: "000000", Style: 1}}})
		styleSummaryHeader, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Color: []string{"#e2e8f0"}, Pattern: 1}, Border: []excelize.Border{{Type: "bottom", Color: "000000", Style: 2}}})
		styleGrandTotal, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true, Size: 12}, Fill: excelize.Fill{Type: "pattern", Color: []string{"#cbd5e1"}, Pattern: 1}, Border: []excelize.Border{{Type: "top", Color: "000000", Style: 2}, {Type: "bottom", Color: "000000", Style: 2}}})

		// Заголовки листа 1
		headersRaw := []string{"Заказ", "SN", "Время", "Участок"}
		for i, h := range headersRaw {
			cell, _ := excelize.CoordinatesToCellName(i+1, 1)
			f.SetCellValue(s, cell, h)
			f.SetCellStyle(s, cell, cell, styleHeader)
		}

		summary := make(map[string]map[string]int)

		for rows.Next() {
			var n, sn, ts, t string
			rows.Scan(&n, &sn, &ts, &t)

			// Заполняем сырые данные
			f.SetCellValue(s, fmt.Sprintf("A%d", idx), n)
			f.SetCellValue(s, fmt.Sprintf("B%d", idx), sn)
			f.SetCellValue(s, fmt.Sprintf("C%d", idx), ts)

			lineName := "Неизв."
			switch t {
			case "conveyor_1":
				lineName = "Линия 1"
			case "conveyor_2":
				lineName = "Линия 2"
			case "stapel":
				lineName = "Стапель"
			}
			f.SetCellValue(s, fmt.Sprintf("D%d", idx), lineName)

			// Собираем статистику для сводной таблицы
			if summary[n] == nil {
				summary[n] = make(map[string]int)
			}
			summary[n][lineName]++
			idx++
		}

		// Ширина колонок
		f.SetColWidth(s, "A", "A", 25)
		f.SetColWidth(s, "B", "B", 30)
		f.SetColWidth(s, "C", "C", 20)
		f.SetColWidth(s, "D", "D", 15)

		// --- СВОДНАЯ ТАБЛИЦА ---
		sumRow := idx + 3
		f.SetCellValue(s, fmt.Sprintf("A%d", sumRow), "СВОДНЫЙ ОТЧЕТ")
		f.SetCellStyle(s, fmt.Sprintf("A%d", sumRow), fmt.Sprintf("A%d", sumRow), styleSummaryHeader)
		sumRow++

		headersSum := []string{"Наименование заказа", "Линия 1", "Линия 2", "Стапель", "ИТОГО"}
		for i, h := range headersSum {
			cell, _ := excelize.CoordinatesToCellName(i+1, sumRow)
			f.SetCellValue(s, cell, h)
			f.SetCellStyle(s, cell, cell, styleHeader)
		}
		sumRow++

		// Сортируем заказы по алфавиту для красоты
		var orderNames []string
		for k := range summary {
			orderNames = append(orderNames, k)
		}
		// Простая пузырьковая сортировка (или можно sort.Strings если импортировать "sort")
		for i := 0; i < len(orderNames)-1; i++ {
			for j := 0; j < len(orderNames)-i-1; j++ {
				if orderNames[j] > orderNames[j+1] {
					orderNames[j], orderNames[j+1] = orderNames[j+1], orderNames[j]
				}
			}
		}

		startSumData := sumRow

		// Переменные для ОБЩЕГО ИТОГА
		var grandL1, grandL2, grandSt, grandAll int

		for _, orderName := range orderNames {
			lines := summary[orderName]
			l1 := lines["Линия 1"]
			l2 := lines["Линия 2"]
			st := lines["Стапель"]
			total := l1 + l2 + st

			// Суммируем в общий итог
			grandL1 += l1
			grandL2 += l2
			grandSt += st
			grandAll += total

			f.SetCellValue(s, fmt.Sprintf("A%d", sumRow), orderName)
			f.SetCellValue(s, fmt.Sprintf("B%d", sumRow), l1)
			f.SetCellValue(s, fmt.Sprintf("C%d", sumRow), l2)
			f.SetCellValue(s, fmt.Sprintf("D%d", sumRow), st)
			f.SetCellValue(s, fmt.Sprintf("E%d", sumRow), total)
			sumRow++
		}

		// Рисуем границы для таблицы данных
		if sumRow > startSumData {
			f.SetCellStyle(s, fmt.Sprintf("A%d", startSumData), fmt.Sprintf("E%d", sumRow-1), styleBorder)
		}

		// --- ВЫВОД СТРОКИ ОБЩЕГО ИТОГА ---
		f.SetCellValue(s, fmt.Sprintf("A%d", sumRow), "ОБЩИЙ ИТОГ")
		f.SetCellValue(s, fmt.Sprintf("B%d", sumRow), grandL1)
		f.SetCellValue(s, fmt.Sprintf("C%d", sumRow), grandL2)
		f.SetCellValue(s, fmt.Sprintf("D%d", sumRow), grandSt)
		f.SetCellValue(s, fmt.Sprintf("E%d", sumRow), grandAll)

		// Стилизуем строку итога (жирный шрифт, фон)
		f.SetCellStyle(s, fmt.Sprintf("A%d", sumRow), fmt.Sprintf("E%d", sumRow), styleGrandTotal)

		c.Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
		c.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", fileName))
		return f.Write(c)
	})

	app.Post("/action/shutdown", func(c *fiber.Ctx) error {
		go func() { time.Sleep(500 * time.Millisecond); os.Exit(0) }()
		return c.JSON(fiber.Map{"status": "stopping"})
	})
	go func() {
		if err := app.Listen(":" + PORT); err != nil {
			app.Listen(":8080")
		}
	}()
	time.Sleep(200 * time.Millisecond)
	w := webview2.New(true)
	defer w.Destroy()
	w.SetTitle("СКТ-ПРО")
	w.SetSize(1200, 800, webview2.HintNone)
	localUrl := "http://localhost"
	if PORT != "80" {
		localUrl += ":" + PORT
	}
	w.Navigate(localUrl)
	w.Run()
}

func getLocalIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

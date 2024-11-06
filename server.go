package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/getlantern/systray"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gopkg.in/ini.v1"
	"gopkg.in/natefinch/lumberjack.v2"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq" // Импортируем драйвер PostgreSQL
)

var (
	mu                   sync.Mutex // Мьютекс для защиты общих переменных
	totalFilesReceived   int
	totalBytesReceived   int64
	lastFileReceivedName string
	lastFileReceivedTime time.Time
	CountAllowedFiles    int
	CountUnknownFiles    int

	// Конфигурационные переменные
	uploadDir string
	host      string
	port      string
	httpsport string
	useHTTPS  bool
	certFile  string
	keyFile   string
	username  string
	password  string
	logDir    string
	logFile   string

	db        *sql.DB // Глобальная переменная для базы данных
	sessionID int     // Переменная для хранения текущего session_id
)

// Statistics Структура для хранения статистики
type Statistics struct {
	SessionID            int       `json:"session_id"`
	SessionStart         time.Time `json:"session_start"`
	SessionEnd           time.Time `json:"session_end"`
	TotalFilesReceived   int       `json:"total_files_received"`
	CountAllowedFiles    int       `json:"count_allowed_files"`
	CountUnknownFiles    int       `json:"count_unknown_files"`
	LastFileReceivedName string    `json:"last_file_name"`
	TotalMBytesReceived  float64   `json:"total_megabyte_received"`
	LastFileReceivedTime time.Time `json:"last_rec_time"`
}

func init() {
	createConfigIfNotExists()

	var err error
	cfg, err := ini.Load("config.ini")
	if err != nil {
		log.Error().Msg(fmt.Sprintf("Error loading file config.ini:  %s", err))
	}

	useHTTPS, _ = cfg.Section("Server").Key("UseHTTPS").Bool()
	host := cfg.Section("Server").Key("Host").String()
	port = cfg.Section("Server").Key("Port").String()
	httpsport = cfg.Section("Server").Key("HTTPS_Port").String()

	if useHTTPS {
		certFile = cfg.Section("Server").Key("CertFile").String()
		keyFile = cfg.Section("Server").Key("KeyFile").String()
		log.Info().Msg(fmt.Sprintf("The server is configured for HTTPS: %s:%s", host, httpsport))
	} else {
		log.Info().Msg(fmt.Sprintf("The server is configured for HTTP. port: %s", port))
	}

	uploadDir = cfg.Section("Server").Key("UploadDir").String()
	port = cfg.Section("Server").Key("Port").String()
	useHTTPS, _ = cfg.Section("Server").Key("UseHTTPS").Bool()
	certFile = cfg.Section("Server").Key("CertFile").String()
	keyFile = cfg.Section("Server").Key("KeyFile").String()
	username = cfg.Section("Auth").Key("Username").String()
	password = cfg.Section("Auth").Key("Password").String()
	logDir = cfg.Section("Log").Key("LogDir").String()
	logFile = cfg.Section("Log").Key("LogFile").String()

	if _, err := os.Stat(uploadDir); os.IsNotExist(err) {
		err := os.MkdirAll(uploadDir, 0755)
		if err != nil {
			return
		}
	}

	//db, err = sql.Open("sqlite3", "./sqlite/statistics.db")
	//if err != nil {
	//	log.Error().Msg(fmt.Sprintf("Ошибка БД>: %v", err))
	//}

	// Подключение к базе данных PostgreSQL
	connStr := "host=localhost user=postgres password=postgres dbname=statistics sslmode=disable" // Замените на ваши данные подключения
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		log.Error().Msg(fmt.Sprintf("Error connecting to PostgreSQL database: %s", err))
	}

	// Создаем таблицу для хранения статистики, если она не существует
	createTableSQL := `CREATE TABLE IF NOT EXISTS statistics (
        session_id SERIAL PRIMARY KEY,
        session_start	TIMESTAMP,
        session_end		TIMESTAMP,
        total_files_received INTEGER,
        count_allowed_files INTEGER,
        count_unknown_files INTEGER,
        last_file_received_name TEXT,
        total_mbytes_received REAL,
        last_file_received_time TIMESTAMP
    );`

	if _, err := db.Exec(createTableSQL); err != nil {
		log.Error().Msg(fmt.Sprintf("Error creating table: %v", err))
	}
	createDirectories()
	recordNewStatistics()
}

// Создание новой записи с новым session_id и нулевыми значениями статистики
func recordNewStatistics() {
	mu.Lock()
	defer mu.Unlock()

	// Создаем новую запись в таблице statistics с нулевыми значениями
	insertSQL := `INSERT INTO statistics (session_start, total_files_received, count_allowed_files, count_unknown_files, last_file_received_name, total_mbytes_received, last_file_received_time) 
				  VALUES (now(), 0, 0, 0, '', 0.0, now()) RETURNING session_id`

	err := db.QueryRow(insertSQL).Scan(&sessionID)
	if err != nil {
		log.Error().Msg(fmt.Sprintf("Error writing initial statistics: %v", err))
	}
}

// Запись статистики в базу данных
func updateStatistics() {
	mu.Lock()
	defer mu.Unlock()

	// Обновляем существующую запись в таблице statistics с текущими значениями статистики
	updateSQL := `UPDATE statistics 
				  SET total_files_received = $1,
					  count_allowed_files = $2,
					  count_unknown_files = $3,
					  last_file_received_name = $4,
					  total_mbytes_received = $5,
					  last_file_received_time = $6 
				  WHERE session_id = $7`

	_, err := db.Exec(updateSQL, totalFilesReceived, CountAllowedFiles, CountUnknownFiles, lastFileReceivedName, float64(totalBytesReceived)/(1024*1024), lastFileReceivedTime, sessionID)

	if err != nil {
		log.Error().Msg(fmt.Sprintf("Error updating statistics: %v", err))
	}
}

func closeDB() {
	setEndSession()                    // Устанавливаем время завершения сессии перед выходом из программы.
	if err := db.Close(); err != nil { // Закрываем базу данных при завершении.
		log.Error().Msg(fmt.Sprintf("Error closing database: %s", err))
	} else {
		log.Info().Msg("Disconnecting from database...")
	}
}

func createConfigIfNotExists() {
	if _, err := os.Stat("config.ini"); os.IsNotExist(err) {
		log.Info().Msg("File config.ini not found. Create new configuration file.")

		cfg := ini.Empty()

		cfg.Section("Server").Key("Port").SetValue("8080")
		cfg.Section("Server").Key("HTTPS_Port").SetValue("443")
		cfg.Section("Server").Key("UploadDir").SetValue("./uploads")
		cfg.Section("Server").Key("UseHTTPS").SetValue("false")
		cfg.Section("Server").Key("CertFile").SetValue("server.crt")
		cfg.Section("Server").Key("KeyFile").SetValue("server.key")

		cfg.Section("Auth").Key("Username").SetValue("admin")
		cfg.Section("Auth").Key("Password").SetValue("password")

		cfg.Section("Log").Key("LogDir").SetValue("./logs")
		cfg.Section("Log").Key("LogFile").SetValue("server.log")

		err := cfg.SaveTo("config.ini")
		if err != nil {
			log.Error().Msg("Error creating config.ini file: ")
		}

		log.Info().Msg("The config.ini file has been successfully created with default settings.")
	}
}

func main() {
	// Настройка ротации логов
	logFilePath := filepath.Join(logDir, logFile)
	logWriter := &lumberjack.Logger{
		Filename:   logFilePath, // Имя файла лога
		MaxSize:    1,           // Максимальный размер файла в МБ
		MaxBackups: 0,           // Максимальное количество резервных файлов
		MaxAge:     0,           // Максимальный возраст резервных файлов в днях
		Compress:   true,        // Сжимать резервные файлы
	}

	// Настройка вывода логов через lumberjack
	log.Logger = log.Output(logWriter)

	zerolog.SetGlobalLevel(zerolog.InfoLevel)

	log.Info().Msg("Starting file receiving server...")
	http.HandleFunc("/", serveIndex) // Главная страница
	http.HandleFunc("/upload", basicAuth(uploadHandler))
	http.HandleFunc("/api/statistics", getStatistics)            // маршрут для получения статистики
	http.HandleFunc("/api/statistics/time", getStatisticsByTime) // маршрут для получения статистики по времени

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		closeDB()
		log.Info().Msg("Terminating file receiving program...")
		os.Exit(0) // Завершение программы
	}()

	go func() {
		systray.Run(onReady, onExit)
		os.Exit(0)
	}()

	if useHTTPS {
		addr := fmt.Sprintf("%s:%s", host, httpsport)
		log.Info().Msg(fmt.Sprintf("Launching HTTPS server on %s", addr))
		err := http.ListenAndServeTLS(addr, certFile, keyFile, nil)
		if err != nil {
			log.Error().Msg("HTTPS server startup error: ")
		}
	} else {
		addr := fmt.Sprintf("%s:%s", host, port)
		log.Info().Msg(fmt.Sprintf("Launching HTTP server on %s", addr))
		err := http.ListenAndServe(addr, nil)
		if err != nil {
			log.Error().Msg("HTTP server startup error: ")
		}
	}
}

func setEndSession() {
	updateSQL := `UPDATE statistics 
					  SET session_end = now()
					  WHERE session_id = $1`
	_, err := db.Exec(updateSQL, sessionID)

	if err != nil {
		log.Error().Msg(fmt.Sprintf("Error updating statistics: %v", err))
	}
}

func onReady() {
	// Загрузка иконки из файла
	iconFile, err := os.Open("icon.ico") // Убедитесь, что файл существует
	if err != nil {
		log.Error().Msg(fmt.Sprintf("Error opening icon file: %s", err))
	}
	defer func() {
		if err := iconFile.Close(); err != nil {
			log.Error().Msg(fmt.Sprintf("Error closing icon file: %s", err))
		}
	}()

	// Чтение файла в байты
	iconBytes, err := io.ReadAll(iconFile)
	if err != nil {
		log.Error().Msg(fmt.Sprintf("Error on read icon file: %s", err))
	}

	// Установка иконки и создание меню
	systray.SetIcon(iconBytes)
	systray.SetTitle("Server")
	systray.SetTooltip("Server control")

	mQuit := systray.AddMenuItem("Exit", "Terminate application")

	go func() {
		for {
			<-mQuit.ClickedCh
			systray.Quit()
		}
	}()
}

func onExit() {
	closeDB()
	log.Info().Msg("Terminate application...")
}

// Обработчик для отображения HTML-страницы
func serveIndex(w http.ResponseWriter, r *http.Request) {
	//log.Info().Msg(fmt.Sprintf("msg: %s", msg)
	http.ServeFile(w, r, "index.html")
}

func createDirectories() {
	dirs := []string{logDir, uploadDir, "unknown"}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Error().Msg(fmt.Sprintf("Ошибка создания директории %s: %v", dir, err))
		}
	}
}

func basicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != username || pass != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	}
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	// Проверка метода запроса
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		log.Warn().Msg(fmt.Sprintf("A request with an unsupported method was received.: %s", r.Method))
		return
	}

	// Получение файла из запроса
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Error receiving file: "+err.Error(), http.StatusBadRequest)
		log.Error().Msg(fmt.Sprintf("Error receiving file: %s", err))
		return
	}
	defer func(file multipart.File) {
		_ = file.Close()
	}(file)

	// Определение папки для сохранения файла
	var saveDir string
	ext := filepath.Ext(header.Filename)
	if ext == ".txt" || ext == ".q" {
		saveDir = uploadDir
	} else {
		saveDir = "unknown"
	}

	// Создаем папку, если она не существует
	if err := os.MkdirAll(saveDir, os.ModePerm); err != nil {
		http.Error(w, "Error creating folder: "+err.Error(), http.StatusInternalServerError)
		log.Error().Msg(fmt.Sprintf("Error creating folder: %s", err))
		return
	}

	// Получаем имя файла без расширения
	filename := strings.TrimSuffix(header.Filename, ext)

	// Создаем новый файл с добавлением "_1", "_2", и т.д. Если файл уже существует
	newFilename := filepath.Join(saveDir, header.Filename)
	i := 1
	for {
		if _, err := os.Stat(newFilename); os.IsNotExist(err) {
			break
		}
		newFilename = filepath.Join(saveDir, fmt.Sprintf("%s_%d%s", filename, i, ext))
		i++
	}

	// Создаем файл на сервере
	dst, err := os.Create(newFilename)
	if err != nil {
		http.Error(w, "Error creating file on server: "+err.Error(), http.StatusInternalServerError)
		log.Error().Msg(fmt.Sprintf("ОError creating file on server: %s", err))
		return
	}

	// Копируем содержимое загруженного файла в новый файл на сервере
	_, err = io.Copy(dst, file)
	if err != nil {
		http.Error(w, "File copy error: "+err.Error(), http.StatusInternalServerError)
		log.Error().Msg(fmt.Sprintf("File copy error: %s", err))
		return
	}

	// Получаем информацию о загруженном файле
	fileInfo, err := os.Stat(dst.Name())
	if err != nil {
		http.Error(w, "Error getting file information: "+err.Error(), http.StatusInternalServerError)
		log.Error().Msg(fmt.Sprintf("Error getting file information: %s", err))
		return
	}

	// Защита общих переменных
	mu.Lock()
	totalFilesReceived++
	totalBytesReceived += fileInfo.Size()
	lastFileReceivedName = header.Filename
	lastFileReceivedTime = time.Now()
	if saveDir == uploadDir {
		CountAllowedFiles++
	} else {
		CountUnknownFiles++
	}
	mu.Unlock()

	updateStatistics() // Обновляем статистику в базе данных
	totalBytesReceivedMB := float64(totalBytesReceived) / (1024 * 1024)

	log.Info().Msg(fmt.Sprintf("File uploaded successfully: %s", lastFileReceivedName))
	fmt.Printf("File uploaded successfully: %s | total files received: %d | total megabyte received: %.2f MB | last file name: %s в %s\n",
		lastFileReceivedName, totalFilesReceived, totalBytesReceivedMB, lastFileReceivedName, lastFileReceivedTime.Format(time.RFC3339))

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("File uploaded successfully"))
}

// Обработчик для получения статистики
func getStatistics(w http.ResponseWriter, _ *http.Request) {
	mu.Lock()
	defer mu.Unlock()

	// Извлекаем последнюю запись из таблицы statistics
	var stats Statistics
	row := db.QueryRow("SELECT session_id, session_start, total_files_received, count_allowed_files, count_unknown_files, last_file_received_name, total_mbytes_received, last_file_received_time FROM statistics ORDER BY session_id DESC LIMIT 1")

	if err := row.Scan(&stats.SessionID, &stats.SessionStart, &stats.TotalFilesReceived, &stats.CountAllowedFiles, &stats.CountUnknownFiles, &stats.LastFileReceivedName, &stats.TotalMBytesReceived, &stats.LastFileReceivedTime); err != nil {
		log.Error().Msg(fmt.Sprintf("Error extracting statistics: %v", err))
		http.Error(w, "Error extracting statistics", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	log.Info().Msg(fmt.Sprintf("Statistics: %+v", stats))

	// Кодируем структуру в JSON и отправляем ответ
	if err := json.NewEncoder(w).Encode(stats); err != nil {
		log.Error().Msg(fmt.Sprintf("JSON encoding error: %v", err))
		http.Error(w, "JSON encoding error", http.StatusInternalServerError)
		return
	}
}

// Обработчик для получения статистики по времени
func getStatisticsByTime(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()

	date := r.URL.Query().Get("date")

	// Параметры для запроса к базе данных
	var stats Statistics

	// Запрос к базе данных для получения статистики по дате
	query := `SELECT session_id, session_start, session_end, total_files_received, count_allowed_files, count_unknown_files, last_file_received_name, total_mbytes_received, last_file_received_time 
              FROM statistics WHERE date(last_file_received_time) = $1`

	err := db.QueryRow(query, date).Scan(&stats.SessionID, &stats.SessionStart, &stats.SessionEnd, &stats.TotalFilesReceived, &stats.CountAllowedFiles, &stats.CountUnknownFiles,
		&stats.LastFileReceivedName, &stats.TotalMBytesReceived, &stats.LastFileReceivedTime)

	w.Header().Set("Content-Type", "application/json")

	if err == sql.ErrNoRows {
		json.NewEncoder(w).Encode(map[string]string{"message": "No data for the specified date"})
		log.Error().Msg(fmt.Sprintf("Error extracting statistics: %v Date: %s", err, date))
		return
	}

	log.Info().Msg(fmt.Sprintf("Statistics_by_time: %+v", stats))

	// Кодируем структуру в JSON и отправляем ответ
	if err := json.NewEncoder(w).Encode(stats); err != nil {
		log.Error().Msg(fmt.Sprintf("JSON encoding error: %v", err))
		http.Error(w, "JSON encoding error", http.StatusInternalServerError)
		return
	}
}

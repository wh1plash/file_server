package main

import (
	"encoding/json"
	"errors"
	"fmt"
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
)

// Statistics Структура для хранения статистики
type Statistics struct {
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
		log.Error().Err(errors.New("Ошибка при загрузке файла config.ini: "))
	}

	useHTTPS, _ = cfg.Section("Server").Key("UseHTTPS").Bool()
	host := cfg.Section("Server").Key("Host").String()
	port = cfg.Section("Server").Key("Port").String()
	httpsport = cfg.Section("Server").Key("HTTPS_Port").String()

	if useHTTPS {
		certFile = cfg.Section("Server").Key("CertFile").String()
		keyFile = cfg.Section("Server").Key("KeyFile").String()
		log.Info().Msg(fmt.Sprintf("Сервер настроен на HTTPS: %s:%s", host, httpsport))
	} else {
		log.Info().Msg(fmt.Sprintf("Сервер настроен на HTTP. port: %s", port))
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
}

func createConfigIfNotExists() {
	if _, err := os.Stat("config.ini"); os.IsNotExist(err) {
		log.Info().Msg("Файл config.ini не найден. Создаем новый файл конфигурации.")

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
			log.Error().Msg("Ошибка при создании файла config.ini: ")
		}

		log.Info().Msg("Файл config.ini успешно создан с настройками по умолчанию.")
	}
}

func main() {
	createDirectories()

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

	log.Info().Msg("Приложение запущено")

	log.Info().Msg("Запуск сервера приема файлов...")
	http.HandleFunc("/upload", basicAuth(uploadHandler))
	http.HandleFunc("/api/statistics", getStatistics) // Добавляем новый маршрут

	// Запись в лог при завершении программы
	exitHandler := func() {
		log.Info().Msg("Завершение программы приема файлов...")
		os.Exit(0)
	}
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		exitHandler()
	}()

	if useHTTPS {
		addr := fmt.Sprintf("%s:%s", host, httpsport)
		log.Info().Msg(fmt.Sprintf("Запуск HTTPS сервера на %s\n", addr))
		err := http.ListenAndServeTLS(addr, certFile, keyFile, nil)
		if err != nil {
			log.Error().Msg("Ошибка запуска HTTPS сервера: ")
		}
	} else {
		addr := fmt.Sprintf("%s:%s", host, port)
		log.Info().Msg(fmt.Sprintf("Запуск HTTP сервера на %s\n", addr))
		err := http.ListenAndServe(addr, nil)
		if err != nil {
			log.Error().Msg("Ошибка запуска HTTP сервера: ")
		}
	}
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
		http.Error(w, "Метод не разрешен", http.StatusMethodNotAllowed)
		log.Warn().Msg(fmt.Sprintf("Получен запрос с неподдерживаемым методом: %s", r.Method))
		return
	}

	// Получение файла из запроса
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Ошибка получения файла: "+err.Error(), http.StatusBadRequest)
		log.Error().Msg("Ошибка получения файла:")
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
		http.Error(w, "Ошибка создания папки: "+err.Error(), http.StatusInternalServerError)
		log.Error().Msg("Ошибка создания папки:")
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
		http.Error(w, "Ошибка создания файла на сервере: "+err.Error(), http.StatusInternalServerError)
		log.Error().Msg("Ошибка создания файла:")
		return
	}

	// Копируем содержимое загруженного файла в новый файл на сервере
	_, err = io.Copy(dst, file)
	if err != nil {
		http.Error(w, "Ошибка копирования файла: "+err.Error(), http.StatusInternalServerError)
		log.Error().Msg("Ошибка копирования файла:")
		return
	}

	// Получаем информацию о загруженном файле
	fileInfo, err := os.Stat(dst.Name())
	if err != nil {
		http.Error(w, "Ошибка получения информации о файле: "+err.Error(), http.StatusInternalServerError)
		log.Error().Msg("Ошибка получения информации о файле:")
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

	totalBytesReceivedMB := float64(totalBytesReceived) / (1024 * 1024)

	log.Info().Msg(fmt.Sprintf("Файл успешно загружен: %s", lastFileReceivedName))
	fmt.Printf("Файл успешно загружен: %s | Количество принятых файлов: %d | Общий объем: %.2f MB | Последний файл: %s в %s\n",
		lastFileReceivedName, totalFilesReceived, totalBytesReceivedMB, lastFileReceivedName, lastFileReceivedTime.Format(time.RFC3339))

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("File uploaded successfully"))
}

// Обработчик для получения статистики
func getStatistics(w http.ResponseWriter, _ *http.Request) {
	mu.Lock()
	defer mu.Unlock()

	totalBytesReceivedMB := float64(totalBytesReceived) / (1024 * 1024)

	stats := Statistics{
		TotalFilesReceived:   totalFilesReceived,
		CountAllowedFiles:    CountAllowedFiles,
		CountUnknownFiles:    CountUnknownFiles,
		LastFileReceivedName: lastFileReceivedName,
		TotalMBytesReceived:  totalBytesReceivedMB,
		LastFileReceivedTime: lastFileReceivedTime,
	}

	log.Info().Msg(fmt.Sprintf("Statistics: %+v", stats)) // Отладочное сообщение

	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(stats)
	if err != nil {
		return
	}
}

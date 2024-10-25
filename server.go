package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"gopkg.in/ini.v1"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	mu                   sync.Mutex // Мьютекс для защиты общих переменных
	lastLogDate          time.Time
	totalFilesReceived   int
	totalBytesReceived   int64
	lastFileReceivedName string
	lastFileReceivedTime time.Time
	CountAllowedFiles    int
	CountUnknownFiles    int

	// Конфигурационные переменные
	uploadDir  string
	host       string
	port       string
	https_port string
	useHTTPS   bool
	certFile   string
	keyFile    string
	username   string
	password   string
	logDir     string
	logFile    string
)

// Структура для хранения статистики
type Statistics struct {
	TotalFilesReceived   int       `json:"total_files_received"`
	CountAllowedFiles    int       `json:"count_allowed_files"`
	CountUnknownFiles    int       `json:"count_unknown_files"`
	LastFileReceivedName string    `json:"last_file_name"`
	TotalMBytesReceived  float64   `json:"total_mbytes_received"`
	LastFileReceivedTime time.Time `json:"last_rec_time"`
}

const (
	maxLogSize = 2 * 1024 * 1024 // 2 MB
)

func init() {
	createConfigIfNotExists()

	var err error
	cfg, err := ini.Load("config.ini")
	if err != nil {
		log.Fatal("Ошибка при загрузке файла config.ini: ", err)
	}

	useHTTPS, _ = cfg.Section("Server").Key("UseHTTPS").Bool()
	host := cfg.Section("Server").Key("Host").String()
	port = cfg.Section("Server").Key("Port").String()
	https_port = cfg.Section("Server").Key("HTTPS_Port").String()

	if useHTTPS {
		certFile = cfg.Section("Server").Key("CertFile").String()
		keyFile = cfg.Section("Server").Key("KeyFile").String()
		log.Printf("Сервер настроен на HTTPS: %s:%s\n", host, https_port)
	} else {
		log.Printf("Сервер настроен на HTTP. port: %s\n", port)
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

	setupLogging()

	if _, err := os.Stat(uploadDir); os.IsNotExist(err) {
		os.MkdirAll(uploadDir, 0755)
	}
}

func createConfigIfNotExists() {
	if _, err := os.Stat("config.ini"); os.IsNotExist(err) {
		log.Println("Файл config.ini не найден. Создаем новый файл конфигурации.")

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
			log.Fatal("Ошибка при создании файла config.ini: ", err)
		}

		log.Println("Файл config.ini успешно создан с настройками по умолчанию.")
	}
}

func main() {
	createDirectories()
	setupLogging()
	logWithCheck(fmt.Sprint("Запуск сервера приема файлов..."))
	http.HandleFunc("/upload", basicAuth(uploadHandler))
	http.HandleFunc("/api/statistics", getStatistics) // Добавляем новый маршрут

	// Запись в лог при завершении программы
	exitHandler := func() {
		log.Println("Завершение программы отправки файлов...")
		os.Exit(0)
	}
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		exitHandler()
	}()

	if useHTTPS {
		addr := fmt.Sprintf("%s:%s", host, https_port)
		log.Printf("Запуск HTTPS сервера на %s\n", addr)
		err := http.ListenAndServeTLS(addr, certFile, keyFile, nil)
		if err != nil {
			log.Fatal("Ошибка запуска HTTPS сервера: ", err)
		}
	} else {
		addr := fmt.Sprintf("%s:%s", host, port)
		log.Printf("Запуск HTTP сервера на %s\n", addr)
		err := http.ListenAndServe(addr, nil)
		if err != nil {
			log.Fatal("Ошибка запуска HTTP сервера: ", err)
		}
	}
}

func createDirectories() {
	dirs := []string{logDir, uploadDir}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Fatalf("Ошибка создания директории %s: %v", dir, err)
		}
	}
}

func setupLogging() {
	// Создаем директорию для логов, если она не существует
	if _, err := os.Stat(logDir); os.IsNotExist(err) {
		os.Mkdir(logDir, 0755)
	}

	// Проверяем, существует ли лог-файл
	logFilePath := filepath.Join(logDir, logFile)
	if _, err := os.Stat(logFilePath); err == nil {
		// Лог-файл существует, проверяем его размер
		fileInfo, err := os.Stat(logFilePath)
		if err == nil && fileInfo.Size() > int64(maxLogSize) {
			// Если размер больше 2 МБ, выполняем ротацию
			rotateLogs(logFilePath, time.Now())
		}
	}

	// Открываем новый лог-файл для записи
	file, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatal("Ошибка открытия файла логов:", err)
	}
	log.SetOutput(file)
}

// Функция для проверки размера лог-файла и архивации
func checkLogSizeAndRotate() {
	logFilePath := filepath.Join(logDir, logFile)
	fileInfo, err := os.Stat(logFilePath)
	if err == nil && fileInfo.Size() > int64(maxLogSize) {
		rotateLogs(logFilePath, time.Now())
	}
}

// Функция для записи в лог с проверкой размера
func logWithCheck(message string) {
	checkLastRowAndRotateBeforeWrite() // Проверяем последнюю строку перед записью
	log.Println(message)
	checkLogSizeAndRotate() // Проверяем размер после записи
}

// Функция для проверки даты последнего сообщения в лог-файле перед записью
func checkLastRowAndRotateBeforeWrite() {
	logFilePath := filepath.Join(logDir, logFile)
	lines, err := readLastLines(logFilePath, 2)
	if err == nil && len(lines) > 0 {
		lastLine := lines[len(lines)-1]
		logDate, err := time.Parse("2006/01/02 15:04:05", lastLine[:19])
		if err == nil && logDate.Before(time.Now().Truncate(24*time.Hour)) {
			// Дата последней записи меньше текущей, выполняем ротацию
			rotateLogs(logFilePath, logDate)
		}
	}
}

// Функция для ротации логов
func rotateLogs(logFilePath string, logDate time.Time) {
	// Создаем директорию для старых логов
	oldLogDir := filepath.Join(logDir, logDate.Format("2006-01-02"))
	os.MkdirAll(oldLogDir, 0755)

	// Перемещаем старый лог-файл
	archivedLogPath := filepath.Join(oldLogDir, logFile)
	err := os.Rename(logFilePath, archivedLogPath)
	if err != nil {
		log.Println("Ошибка перемещения лог-файла:", err)
		return
	}

	// Определяем имя архива
	baseTarFileName := logDate.Format("2006-01-02")
	tarFileName := filepath.Join(oldLogDir, fmt.Sprintf("%s-1.tar.gz", baseTarFileName))

	// Проверяем существование архива и увеличиваем номер, если необходимо
	n := 1
	for {
		if _, err := os.Stat(tarFileName); os.IsNotExist(err) {
			break // Файл не существует, можно использовать это имя
		}
		n++
		tarFileName = filepath.Join(oldLogDir, fmt.Sprintf("%s-%d.tar.gz", baseTarFileName, n))
	}

	// Архивируем лог-файл
	cmd := exec.Command("tar", "-czf", tarFileName, "-C", oldLogDir, logFile)
	if err := cmd.Run(); err != nil {
		log.Println("Ошибка при архивировании логов:", err)
		return
	}

	// Удаляем старый лог-файл после успешного архивирования
	os.Remove(archivedLogPath)
	log.Println("Логи успешно архивированы в:", tarFileName)

	// Инициализация нового лога
	setupLogging()
}

func readLastLines(filePath string, n int) ([]string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > n {
			lines = lines[1:] // Удаляем старые строки, если их больше n
		}
	}
	return lines, scanner.Err()
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
		log.Println("Получен запрос с неподдерживаемым методом:", r.Method)
		return
	}

	// Получение файла из запроса
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Ошибка получения файла: "+err.Error(), http.StatusBadRequest)
		log.Println("Ошибка получения файла:", err)
		return
	}
	defer file.Close()

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
		log.Println("Ошибка создания папки:", err)
		return
	}

	// Получаем имя файла без расширения
	filename := strings.TrimSuffix(header.Filename, ext)

	// Создаем новый файл с добавлением "_1", "_2", и т.д. если файл уже существует
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
		log.Println("Ошибка создания файла:", err)
		return
	}

	// Копируем содержимое загруженного файла в новый файл на сервере
	_, err = io.Copy(dst, file)
	if err != nil {
		http.Error(w, "Ошибка копирования файла: "+err.Error(), http.StatusInternalServerError)
		log.Println("Ошибка копирования файла:", err)
		return
	}

	// Получаем информацию о загруженном файле
	fileInfo, err := os.Stat(dst.Name())
	if err != nil {
		http.Error(w, "Ошибка получения информации о файле: "+err.Error(), http.StatusInternalServerError)
		log.Println("Ошибка получения информации о файле:", err)
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

	logWithCheck(fmt.Sprintf("Файл успешно загружен: %s", lastFileReceivedName))
	fmt.Printf("Файл успешно загружен: %s | Количество принятых файлов: %d | Общий объем: %.2f MB | Последний файл: %s в %s\n",
		lastFileReceivedName, totalFilesReceived, totalBytesReceivedMB, lastFileReceivedName, lastFileReceivedTime.Format(time.RFC3339))

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("File uploaded successfully"))
}

// Обработчик для получения статистики
func getStatistics(w http.ResponseWriter, r *http.Request) {
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

	//log.Printf("Statistics: %+v\n", stats) // Отладочное сообщение

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

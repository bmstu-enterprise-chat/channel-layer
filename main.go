package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"time"
)

/*

		      +----------------------+
		      |  HTTP /code запрос   |
		      | (JSON с payload)     |
		      +----------+-----------+
					     |
					     v
		      +----------+-----------+
		      | handleCode: парсинг  |
		      | IncomingCodeRequest  |
		      +----------+-----------+
					     |
			 		     v
		   +-------------+--------------+
		   | Формирование Segment       |
		   | (FixedPayloadSize байт)    |
		   +-------------+--------------+
					     |
	   				     v
      +------------------+------------------+
      |   ChannelLayer.ProcessSegment       |
      +------------------+------------------+
		  			     |
	+--------------------+---------------------+
	| Кодирование [7,4] каждого блока (280 шт.)|
	+--------------------+---------------------+
					     |
	+--------------------+---------------------+
	| Симуляция потери кадра (LossProbability) |
	+--------------------+---------------------+
						 |
	+--------------------+----------------------+
	| Симуляция ошибки в бите (ErrorProbability)|
	+--------------------+----------------------+
						 |
	+--------------------+---------------------+
	| Декодирование [7,4] каждого блока        |
	| Проверка на ошибки (по синдрому)         |
	+--------------------+---------------------+
						 |
	+--------------------+---------------------+
	| Формирование Segment c результатом       |
	| (и флагом IsChannelError)                |
	+--------------------+---------------------+
						 |
						 v
			  +----------+------------+
			  | handleCode: формирует |
			  | HTTP /transfer запрос |
			  +----------+------------+
						 |
						 v
			  +----------+-----------+
			  | HTTP POST на         |
			  | TransferURL          |
			  +----------------------+

*/

// Определение констант для лучшей читаемости и легкого изменения
const (
	ListenPort         = ":8081"                       // Порт, на котором слушает веб-сервер
	TransferEndpoint   = "/transfer"                   // Конечная точка для пересылки данных
	CodeEndpoint       = "/code"                       // Конечная точка для приема входных данных
	TransferURL        = "http://localhost:8080/transfer" // Полный URL целевого сервера (предполагается, что он запущен на 8080)
	FixedPayloadSize   = 140                           // X: Фиксированный размер полезной нагрузки в байтах (после паддинга/до кодирования)
	InfoBitsPerBlock   = 4                             // k: Количество информационных бит в блоке для кода [7,4]
	CodedBitsPerBlock  = 7                             // n: Количество кодовых бит в блоке для кода [7,4]
	PayloadBitLength   = FixedPayloadSize * 8          // Общее количество бит в полезной нагрузке (после паддинга)
	NumCodingBlocks    = PayloadBitLength / InfoBitsPerBlock // Количество блоков [7,4] для кодирования (1120 / 4 = 280 блоков)
	EncodedBitLength   = NumCodingBlocks * CodedBitsPerBlock // Общее количество бит после кодирования (280 * 7 = 1960 бит)
)

// Segment представляет собой сегмент данных, передаваемый между уровнями.
// Используется только внутри ChannelLayer.
type Segment struct {
	Payload       []byte `json:"payload"`         // Полезная нагрузка (часть текста или файла). Всегда FixedPayloadSize байт после паддинга.
	Timestamp     int64  `json:"timestamp"`     // Временная метка отправителя (часть ID сообщения) в наносекундах.
	TotalSegments int    `json:"total_segments"` // Общее количество сегментов для исходного сообщения
	SegmentNumber int    `json:"segment_number"` // Порядковый номер данного сегмента (начинается с 1)
	// IsChannelError устанавливается Канальным уровнем, если декодирование сегмента не удалось
	// (обнаружена неисправимая ошибка). Транспортный уровень может добавить свой флаг
	// ошибки, если сегменты сообщения потеряны.
	IsChannelError bool `json:"is_channel_error"`
}

// IncomingCodeRequest структура для парсинга входящего JSON на /code
type IncomingCodeRequest struct {
	SegmentNumber int    `json:"segment_number"`
	TotalSegments int    `json:"total_segments"`
	Sender        string `json:"sender"`
	SendTime      string `json:"send_time"` // Приходит как строка
	Payload       string `json:"payload"`       // Приходит как строка (может быть до FixedPayloadSize байт)
}

// OutgoingTransferRequest структура для формирования исходящего JSON на /transfer
type OutgoingTransferRequest struct {
	SegmentNumber int    `json:"segment_number"`
	TotalSegments int    `json:"total_segments"`
	Sender        string `json:"sender"`
	SendTime      string `json:"send_time"` // Отправляется как строка, как пришло
	Payload       string `json:"payload"`       // Отправляется как строка (всегда FixedPayloadSize байт после паддинга и обработки)
}

// APIError структура для стандартизированного ответа при ошибке
type APIError struct {
	Error string `json:"error"`
}

// ChannelLayer симулирует ненадежный канал связи с потерями и ошибками в битах.
type ChannelLayer struct {
	ErrorProbability float64 // P: Вероятность ошибки в бите передаваемого *закодированного* кадра
	LossProbability  float64 // R: Вероятность потери всего *закодированного* кадра
	rng              *rand.Rand // Собственный генератор случайных чисел для изоляции
}

// NewChannelLayer создает новый экземпляр Канального уровня с заданными вероятностями.
func NewChannelLayer(errorProb, lossProb float64) *ChannelLayer {
	// Использование NewSource с UnixNano обеспечивает более случайный начальный сид.
	source := rand.NewSource(time.Now().UnixNano())
	rng := rand.New(source)

	log.Printf("ChannelLayer: Создан с вероятностью ошибки бита P=%.4f и вероятностью потери кадра R=%.4f", errorProb, lossProb)

	return &ChannelLayer{
		ErrorProbability: errorProb,
		LossProbability:  lossProb,
		rng:              rng,
	}
}

// ProcessSegment симулирует передачу сегмента через зашумленный канал.
// Принимает сегмент (от Транспортного уровня), обрабатывает его (кодирование, симуляция
// ошибок/потерь, декодирование) и возвращает обработанный сегмент (для Транспортного уровня)
// или nil, если кадр был потерян.
// Принимает внутреннюю структуру Segment с []byte payload и int64 Timestamp.
// Ожидает payload РОВНО FixedPayloadSize байт после возможного паддинга.
func (cl *ChannelLayer) ProcessSegment(inputSegment *Segment) *Segment {
	log.Printf("ChannelLayer: Принят сегмент #%d/%d (timestamp %d), размер полезной нагрузки %d байт",
		inputSegment.SegmentNumber, inputSegment.TotalSegments, inputSegment.Timestamp, len(inputSegment.Payload))

	// Проверка размера входной полезной нагрузки: должна быть ровно FixedPayloadSize
	if len(inputSegment.Payload) != FixedPayloadSize {
		log.Printf("ChannelLayer ERROR: Внутренняя ошибка: Неожиданный размер полезной нагрузки после паддинга: %d байт, ожидалось %d. Помечаем как ошибку канала.",
			len(inputSegment.Payload), FixedPayloadSize)
		// Это индикатор проблемы в предыдущем слое (handleCode), но для симуляции
		// помечаем это как неисправимую ошибку канала, так как обработка невозможна.
		outputSegment := &Segment{
			Payload:        nil, // Payload не может быть обработан
			Timestamp:      inputSegment.Timestamp,
			TotalSegments:  inputSegment.TotalSegments,
			SegmentNumber:  inputSegment.SegmentNumber,
			IsChannelError: true, // Помечаем как неисправимую ошибку канала
		}
		return outputSegment
	}

	// 1. Кодирование полезной нагрузки с использованием кода [7,4]
	// Преобразуем байты полезной нагрузки в поток битов.
	bitStreamIn := bytesToBitStream(inputSegment.Payload) // FixedPayloadSize * 8 бит = 1120 бит

	if len(bitStreamIn) != PayloadBitLength {
         log.Printf("ChannelLayer ERROR: Внутренняя ошибка: Неверная длина потока битов после преобразования байт (%d), ожидалось %d. Помечаем как ошибку канала.", len(bitStreamIn), PayloadBitLength)
          outputSegment := &Segment{
             Payload:        nil,
             Timestamp:      inputSegment.Timestamp,
             TotalSegments:  inputSegment.TotalSegments,
             SegmentNumber:  inputSegment.SegmentNumber,
             IsChannelError: true,
         }
         return outputSegment
	}

	// Выделяем память под закодированный поток битов. Каждый блок из 4 бит кодируется в 7 бит.
	encodedBitStream := make([]uint8, EncodedBitLength) // NumCodingBlocks * CodedBitsPerBlock = 280 * 7 = 1960 бит

	// Проходим по каждому блоку из 4 информационных бит и кодируем его.
	for i := 0; i < NumCodingBlocks; i++ {
		// Выбираем текущий блок информационных бит
		blockIn := bitStreamIn[i*InfoBitsPerBlock : (i+1)*InfoBitsPerBlock]
		// Кодируем блок
		blockOut := cyclicEncode7_4Block(blockIn)
		// Копируем результат кодирования (7 бит) в закодированный поток
		copy(encodedBitStream[i*CodedBitsPerBlock:(i+1)*CodedBitsPerBlock], blockOut)
	}
	log.Printf("ChannelLayer: Закодировано %d бит в %d бит (блоков [7,4]: %d)", PayloadBitLength, EncodedBitLength, NumCodingBlocks)


	// 2. Симуляция потери кадра
	if cl.rng.Float64() <= cl.LossProbability {
		log.Printf("ChannelLayer: Симуляция потери кадра для сегмента #%d/%d",
			inputSegment.SegmentNumber, inputSegment.TotalSegments)
		return nil // Кадр (весь закодированный сегмент) потерян
	}

	// 3. Симуляция ошибки в бите (только если кадр не потерян)
	// С вероятностью ErrorProbability, инвертируем один случайный бит в *закодированном* потоке.
	if cl.rng.Float64() <= cl.ErrorProbability {
		// Выбираем случайный индекс бита в закодированном потоке (длиной EncodedBitLength)
		errorBitIndex := cl.rng.Intn(EncodedBitLength)
		// Инвертируем бит: если 0, становится 1; если 1, становится 0.
		encodedBitStream[errorBitIndex] = 1 - encodedBitStream[errorBitIndex]
		log.Printf("ChannelLayer: Симуляция ошибки в бите по индексу %d в закодированном потоке", errorBitIndex)
	} else {
         log.Println("ChannelLayer: Ошибка в бите не симулирована.")
    }


	// 4. Декодирование полезной нагрузки с использованием кода [7,4]
	// Выделяем память под декодированный поток битов (должен быть такого же размера, как и исходный поток битов)
	decodedBitStream := make([]uint8, PayloadBitLength)
	channelErrorDetected := false // Флаг для обнаружения неисправимых ошибок

	// Проходим по каждому блоку из 7 принятых битов и декодируем его.
	for i := 0; i < NumCodingBlocks; i++ {
		// Выбираем текущий блок принятых битов (который мог содержать ошибки)
		blockIn := encodedBitStream[i*CodedBitsPerBlock : (i+1)*CodedBitsPerBlock]
		// Декодируем блок. Функция пытается обнаружить ошибки.
		blockOut, detectedError := cyclicDecode7_4Block(blockIn)
		// Копируем результат декодирования (4 бита, независимо от того, была ли ошибка) в декодированный поток
		copy(decodedBitStream[i*InfoBitsPerBlock:(i+1)*InfoBitsPerBlock], blockOut)
		// Если декодер обнаружил ошибку в этом блоке, устанавливаем общий флаг ошибки канала.
		if detectedError {
			channelErrorDetected = true // Обнаружена неисправимая ошибка в одном из блоков
		}
	}
	log.Printf("ChannelLayer: Декодировано %d бит обратно в %d бит", EncodedBitLength, PayloadBitLength)

	// Преобразуем декодированный поток битов обратно в байты.
	decodedPayload := bitStreamToBytes(decodedBitStream)

	// Проверка, что декодированный payload имеет правильный размер (после обратного преобразования из битов).
	if len(decodedPayload) != FixedPayloadSize {
        log.Printf("ChannelLayer ERROR: Внутренняя ошибка: Неверная длина полезной нагрузки после декодирования битов (%d), ожидалось %d. Помечаем как ошибку канала.", len(decodedPayload), FixedPayloadSize)
        channelErrorDetected = true // Считаем это неисправимой ошибкой
         outputSegment := &Segment{
             Payload:        nil, // Payload не может быть корректным
             Timestamp:      inputSegment.Timestamp,
             TotalSegments:  inputSegment.TotalSegments,
             SegmentNumber:  inputSegment.SegmentNumber,
             IsChannelError: true,
         }
         return outputSegment
    }

    // Создаем итоговый сегмент с декодированной полезной нагрузкой и флагом ошибки.
	outputSegment := &Segment{
		Payload:       decodedPayload,
		Timestamp:     inputSegment.Timestamp,
		TotalSegments: inputSegment.TotalSegments,
		SegmentNumber: inputSegment.SegmentNumber,
		IsChannelError: channelErrorDetected, // Устанавливаем флаг на основе результатов декодирования
	}

	if channelErrorDetected {
		log.Println("ChannelLayer: Обнаружена неисправимая ошибка при декодировании.")
	} else {
		log.Println("ChannelLayer: Декодирование успешно (ошибка отсутствовала или была исправлена).")
	}

	return outputSegment
}

// cyclicEncode7_4Block кодирует 4 информационных бита в 7 кодовых бит, используя циклический код [7,4].
// Этот код определяется генераторным многочленом g(x) = x^3 + x + 1.
// Информационное слово i(x) представляется битами i3 i2 i1 i0 (соответствующими x^3 x^2 x^1 x^0).
// Кодовое слово c(x) = i(x) * x^3 + r(x), где r(x) = i(x) * x^k mod g(x) (здесь k=4).
// В нашем случае, i(x) = i3*x^3 + i2*x^2 + i1*x^1 + i0*x^0.
// c(x) = i3*x^6 + i2*x^5 + i1*x^4 + i0*x^3 + r2*x^2 + r1*x^1 + r0*x^0.
// Расчет проверочных битов (r2, r1, r0) происходит как остаток от деления i(x)*x^3 на g(x)
// (все вычисления по модулю 2).
// r0 = i0 + i1 + i3  (сложение по модулю 2, или XOR)
// r1 = i0 + i2 + i3
// r2 = i1 + i2 + i3
// Кодовое слово имеет структуру (i3, i2, i1, i0, r2, r1, r0).
func cyclicEncode7_4Block(infoBits []uint8) []uint8 {
    // Проверка длины входных данных, хотя на практике здесь всегда должно быть InfoBitsPerBlock (4 бита)
	if len(infoBits) != InfoBitsPerBlock {
        log.Printf("ChannelLayer ERROR: Внутренняя ошибка: Неверная длина входного блока для кодера [7,4]: %d бит, ожидалось %d. Возвращаем нулевой блок.", len(infoBits), InfoBitsPerBlock)
		return make([]uint8, CodedBitsPerBlock) // Возвращаем нулевой блок при ошибке
	}
	// Информационные биты: i3 i2 i1 i0
	i3, i2, i1, i0 := infoBits[0], infoBits[1], infoBits[2], infoBits[3]

	// Расчет проверочных битов (в соответствии с генераторным многочленом x^3 + x + 1)
	// Вычисления проводятся по модулю 2, что эквивалентно операции XOR (^) для битов.
	r0 := i0 ^ i1 ^ i3
	r1 := i0 ^ i2 ^ i3
	r2 := i1 ^ i2 ^ i3

	// Формирование кодового слова: (i3, i2, i1, i0, r2, r1, r0)
	return []uint8{i3, i2, i1, i0, r2, r1, r0}
}

// cyclicDecode7_4Block декодирует 7 принятых битов, используя циклический код [7,4].
// Эта функция вычисляет синдром для обнаружения ошибок, но не пытается их исправить.
// Принятое кодовое слово v(x) = v6*x^6 + v5*x^5 + v4*x^4 + v3*x^3 + v2*x^2 + v1*x^1 + v0*x^0.
// Синдром S(x) = v(x) mod g(x), где g(x) = x^3 + x + 1.
// Синдром представляется битами s2 s1 s0.
// s0 = v0 + v3 + v4 + v6  (сложение по модулю 2, или XOR)
// s1 = v1 + v3 + v5 + v6
// s2 = v2 + v4 + v5 + v6
// Если синдром (s2, s1, s0) = (0, 0, 0), то принятое слово является допустимым кодовым словом (ошибок нет,
// или имеется неисправимая комбинация ошибок, дающая нулевой синдром).
// Если синдром не равен (0, 0, 0), это означает, что была обнаружена ошибка.
// Этот код [7,4] с g(x) = x^3+x+1 может детектировать все одиночные и двойные ошибки.
// Текущая реализация просто использует факт, что ненулевой синдром означает обнаружение ошибки.
// Она *не* реализует логику исправления одиночной ошибки (которая была бы возможна для этого кода
// путем сопоставления ненулевого синдрома с позицией ошибки).
// Декодированные информационные биты просто берутся из соответствующих позиций принятого слова (v6, v5, v4, v3).
func cyclicDecode7_4Block(codedBits []uint8) ([]uint8, bool) {
	if len(codedBits) != CodedBitsPerBlock {
		log.Printf("ChannelLayer ERROR: Внутренняя ошибка: Неверная длина входного блока для декодера [7,4]: %d бит, ожидалось %d.", len(codedBits), CodedBitsPerBlock)
		return make([]uint8, InfoBitsPerBlock), true // Возвращаем нулевые информационные биты и флаг ошибки
	}
	// Принятое кодовое слово (возможно, с ошибками): v6 v5 v4 v3 v2 v1 v0
	v6, v5, v4, v3, v2, v1, v0 := codedBits[0], codedBits[1], codedBits[2], codedBits[3], codedBits[4], codedBits[5], codedBits[6]

	// Расчет синдрома S = (s2, s1, s0) по модулю 2.
	// s0 = v0 + v3 + v4 + v6
	// s1 = v1 + v3 + v5 + v6
	// s2 = v2 + v4 + v5 + v6
	s0 := v0 ^ v3 ^ v4 ^ v6
	s1 := v1 ^ v3 ^ v5 ^ v6
	s2 := v2 ^ v4 ^ v5 ^ v6

	// Проверяем, равен ли синдром нулю.
	syndromeIsZero := (s0 == 0) && (s1 == 0) && (s2 == 0)

	// Ошибка обнаружена, если синдром не равен нулю.
	detectedError := !syndromeIsZero

    // Декодированные информационные биты берутся из принятых битов на позициях информационных битов.
    // В этой реализации декодер не исправляет ошибки, поэтому просто возвращает принятые биты.
    // Если бы была коррекция, эти биты могли бы быть изменены на основе синдрома.
    decodedInfoBits := []uint8{v6, v5, v4, v3}

	return decodedInfoBits, detectedError
}

// bytesToBitStream преобразует срез байт в срез битов (uint8, где 0 или 1).
// Каждый байт (8 бит) преобразуется в 8 элементов среза uint8.
// Старший бит каждого байта (слева) становится первым элементом в соответствующей группе из 8 битов в потоке.
// Например, байт 0b10110100 преобразуется в срез {1, 0, 1, 1, 0, 1, 0, 0}.
func bytesToBitStream(data []byte) []uint8 {
	bitStream := make([]uint8, len(data)*8)
	for i, b := range data {
		for j := 0; j < 8; j++ {
			// Извлекаем j-й бит (считая с 0 для старшего бита слева, т.е. 7-j) из байта 'b'.
			// Сдвигаем бит вправо (7-j) позиций, чтобы он оказался в младшей позиции, и берем его (& 1).
			bit := (b >> (7 - j)) & 1
			// Записываем бит в соответствующую позицию в потоке битов.
			bitStream[i*8+j] = bit
		}
	}
	return bitStream
}

// bitStreamToBytes преобразует срез битов (uint8) обратно в срез байт.
// Каждый байт формируется из 8 последовательных битов из входного потока.
// Первый бит из группы 8 в потоке становится старшим битом (слева) в байте.
// Длина потока битов должна быть кратна 8. Избыточные биты в конце будут отброшены с предупреждением.
func bitStreamToBytes(bitStream []uint8) []byte {
	if len(bitStream)%8 != 0 {
		log.Printf("ChannelLayer WARNING: Длина потока битов (%d) не кратна 8. Обрезаем до %d.", len(bitStream), len(bitStream)/8*8)
		bitStream = bitStream[:len(bitStream)/8*8] // Обрезаем, чтобы длина была кратна 8
	}
	byteData := make([]byte, len(bitStream)/8)
	for i := 0; i < len(byteData); i++ {
		var b byte // Текущий собираемый байт, инициализирован нулем
		for j := 0; j < 8; j++ {
			// Берем j-й бит из текущей группы 8 битов в потоке.
			if bitStream[i*8+j] == 1 {
				// Если бит равен 1, устанавливаем соответствующий бит в байте 'b'.
				// Старший бит потока (j=0) идет в 7-ю позицию байта (1 << 7),
				// следующий бит (j=1) идет в 6-ю позицию (1 << 6), и так далее.
				b |= (1 << (7 - j)) // Устанавливаем бит в позиции (7-j)
			}
		}
		byteData[i] = b // Сохраняем собранный байт
	}
	return byteData
}

var channelLayer *ChannelLayer // Глобальный экземпляр канального уровня

// handleCode обрабатывает входящие POST запросы на /code
func handleCode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req IncomingCodeRequest
	decoder := json.NewDecoder(r.Body)
	// Ограничиваем размер читаемого тела запроса, чтобы избежать злонамеренных запросов
	// (например, отправка очень большого payload)
    // Допустим, максимальный размер payload + JSON оверхед не должен превышать некий лимит.
    // Учитывая, что payload сам по себе до 140 байт, разумный лимит может быть, например, 1KB.
	r.Body = http.MaxBytesReader(w, r.Body, 1024) // Ограничение до 1 KB
	if err := decoder.Decode(&req); err != nil {
        // Проверяем, не была ли ошибка из-за превышения лимита
        if _, ok := err.(*http.MaxBytesError); ok {
             sendErrorResponse(w, fmt.Sprintf("Request body too large. Maximum allowed is %d bytes.", 1024), http.StatusRequestEntityTooLarge)
             return
        }
		sendErrorResponse(w, fmt.Sprintf("Failed to decode JSON request: %v", err), http.StatusBadRequest)
		return
	}

    // Валидация размера полезной нагрузки: должна быть больше 0 и не более FixedPayloadSize
    originalPayloadBytes := []byte(req.Payload)
	if len(originalPayloadBytes) == 0 {
		sendErrorResponse(w, "Invalid payload size: payload cannot be empty.", http.StatusBadRequest)
		return
	}
    if len(originalPayloadBytes) > FixedPayloadSize {
        sendErrorResponse(w, fmt.Sprintf("Invalid payload size: expected %d bytes or less, got %d. Payload size exceeds maximum allowed.", FixedPayloadSize, len(originalPayloadBytes)), http.StatusBadRequest)
        return
    }


	// --- Паддинг полезной нагрузки до FixedPayloadSize байт ---
	paddedPayloadBytes := make([]byte, FixedPayloadSize)
	// Копируем оригинальные данные в начало нового среза.
	// Остаток среза будет заполнен нулевыми байтами (\x00) по умолчанию, что является стандартным методом паддинга.
	copy(paddedPayloadBytes, originalPayloadBytes)
	// ---------------------------------------------

	// Парсинг строки send_time в time.Time
	// Пытаемся распарсить в формате RFC3339 (рекомендуется для обмена данными через API)
	parsedTime, err := time.Parse(time.RFC3339, req.SendTime)
	if err != nil {
		// Если RFC3339 не сработал, пробуем исходный формат из примера на случай, если используется он
		parsedTime, err = time.Parse("2006-01-02 15:04:05 -0700 MST", req.SendTime)
		if err != nil {
			sendErrorResponse(w, fmt.Sprintf("Failed to parse send_time '%s': %v. Expected format like RFC3339 (e.g., '2006-01-02T15:04:05Z') or '2006-01-02 15:04:05 -0700 MST'.", req.SendTime, err), http.StatusBadRequest)
			return
		}
	}

	// Подготовка внутренней структуры Segment для обработки ChannelLayer
	internalSegment := &Segment{
		Payload:       paddedPayloadBytes,       // Используем паддированную полезную нагрузку (FixedPayloadSize байт)
		Timestamp:     parsedTime.UnixNano(),    // Используем метку времени в наносекундах
		TotalSegments: req.TotalSegments,
		SegmentNumber: req.SegmentNumber,
		// IsChannelError будет установлен ChannelLayer
	}

	log.Printf("Web Server: Принят сегмент #%d/%d от %s, обработка с полезной нагрузкой размера %d (ориг. %d)...",
		req.SegmentNumber, req.TotalSegments, req.Sender, len(internalSegment.Payload), len(originalPayloadBytes))

	// Обработка сегмента с использованием ChannelLayer
	processedSegment := channelLayer.ProcessSegment(internalSegment)

	// Проверка результатов обработки
	if processedSegment == nil {
		// Сегмент был потерян
		log.Printf("Web Server: Сегмент #%d/%d потерян во время симуляции канала.", req.SegmentNumber, req.TotalSegments)
		sendErrorResponse(w, "Segment lost during channel simulation", http.StatusRequestTimeout) // или другой подходящий статус
		return
	}

	if processedSegment.IsChannelError {
		// Канальный уровень обнаружил неисправимую ошибку
		log.Printf("Web Server: Канальный уровень обнаружил неисправимую ошибку для сегмента #%d/%d.", req.SegmentNumber, req.TotalSegments)
		sendErrorResponse(w, "Uncorrectable channel error detected during processing", http.StatusInternalServerError) // или StatusBadRequest в зависимости от интерпретации
		return
	}

	// Обработка прошла успешно (или ошибка была исправлена/отсутствовала).
	// Теперь готовим и отправляем сегмент на конечную точку /transfer.

	// ВАЖНО: Исходящий JSON должен использовать *оригинальные* поля из входящего запроса,
	// кроме полезной нагрузки, которая берется из processedSegment (преобразована обратно в строку).
	// Обработанная полезная нагрузка всегда будет FixedPayloadSize байт (преобразована в строку).
	outgoingPayloadString := string(processedSegment.Payload)

	outgoingRequest := OutgoingTransferRequest{
		SegmentNumber: req.SegmentNumber,       // Используем оригинал
		TotalSegments: req.TotalSegments,       // Используем оригинал
		Sender:        req.Sender,             // Используем оригинал
		SendTime:      req.SendTime,           // Используем оригинальный строковый формат
		Payload:       outgoingPayloadString, // Используем обработанную и паддированную полезную нагрузку (как строку, всегда FixedPayloadSize символов/байт)
	}

	outgoingJSON, err := json.Marshal(outgoingRequest)
	if err != nil {
		sendErrorResponse(w, fmt.Sprintf("Failed to marshal outgoing JSON: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("Web Server: Обработка успешна, отправка сегмента #%d/%d на %s с размером полезной нагрузки %d",
		req.SegmentNumber, req.TotalSegments, TransferURL, len(outgoingRequest.Payload))

	// Отправка POST запроса на конечную точку /transfer
	resp, err := http.Post(TransferURL, "application/json", bytes.NewBuffer(outgoingJSON))
	if err != nil {
		// Ошибка при отправке запроса на целевой сервер
		log.Printf("Web Server ERROR: Не удалось отправить сегмент на целевую конечную точку (%s): %v", TransferURL, err)
		// В соответствии с инструкцией, если обработка прошла успешно, отвечаем OK, но логируем ошибку отправки.
        // Однако, в реальной системе здесь, вероятно, стоило бы вернуть ошибку клиенту.
        // Давайте придерживаться текущей логики: если *обработка* успешна, отвечаем OK.
        w.WriteHeader(http.StatusOK) // Успех обработки, несмотря на ошибку отправки
        json.NewEncoder(w).Encode(map[string]string{"status": "Processing successful, but failed to send to transfer endpoint", "error": err.Error()})
        log.Printf("Web Server: Ответили на /code для сегмента #%d/%d со статусом OK (но с ошибкой отправки)", req.SegmentNumber, req.TotalSegments)
		return
	}
	defer resp.Body.Close()

	// Чтение ответа от конечной точки /transfer (опционально, для логирования/отладки)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Web Server WARNING: Не удалось прочитать тело ответа от конечной точки /transfer: %v", err)
	} else {
		log.Printf("Web Server: Получен ответ от конечной точки /transfer (Status: %s): %s", resp.Status, string(body))
	}

	// Отвечаем исходному клиенту на /code, указывая на успешную обработку и попытку отправки
	// Как указано в инструкциях, если обработка прошла успешно, отвечаем OK, независимо от статуса ответа целевого сервера.
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "Processing successful, transfer attempted", "transfer_status": resp.Status}) // Добавляем статус от transfer
	log.Printf("Web Server: Ответили на /code для сегмента #%d/%d со статусом OK (статус transfer: %s)", req.SegmentNumber, req.TotalSegments, resp.Status)
}

// sendErrorResponse отправляет стандартизированный JSON ответ с ошибкой и логирует ее.
func sendErrorResponse(w http.ResponseWriter, message string, statusCode int) {
	log.Printf("Web Server: Отправка ответа с ошибкой (Статус %d): %s", statusCode, message)
	w.WriteHeader(statusCode)
	errorResponse := APIError{Error: message}
	json.NewEncoder(w).Encode(errorResponse)
}


func main() {
	// Инициализация канального уровня с заданными вероятностями ошибки и потери
	// При необходимости эти значения можно вынести в аргументы командной строки или файл конфигурации.
	channelLayer = NewChannelLayer(0.05, 0.01) // Пример: P=0.05 (5% ошибки в бите), R=0.01 (1% потери кадра)

	log.Println("--- Запуск веб-сервера на", ListenPort, "---")
	log.Println("Прослушивание POST запросов на", CodeEndpoint)
	log.Printf("Обработанные сегменты будут пересылаться на %s", TransferURL)

	// Регистрация обработчика для конечной точки /code
	http.HandleFunc(CodeEndpoint, handleCode)

	// Запуск HTTP сервера. log.Fatalf вызывается при фатальной ошибке (например, порт уже занят).
	err := http.ListenAndServe(ListenPort, nil)
	if err != nil {
		log.Fatalf("Не удалось запустить сервер: %v", err)
	}
}
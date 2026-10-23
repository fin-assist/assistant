package domain

import "time"

// GenerationTask identifies one committed answer. Identity and transfer mode come
// from the server's saved question, never from model tool arguments.
type GenerationTask struct {
	UserID, QuestionID, AnswerID, Text, TransferMode, TimeZone string
	QuestionCreatedAt, CreatedAt                               time.Time
}

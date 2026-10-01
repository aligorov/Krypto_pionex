package telegram

import "context"

func (d *OutboxDispatcher) dropWebhook(ctx context.Context, token string) error {
	return callTelegram(ctx, d.httpClient, token, "deleteWebhook", map[string]any{"drop_pending_updates": false}, nil)
}

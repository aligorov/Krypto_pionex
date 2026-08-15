# Настройка Fail2ban для Pionex Control

В бот уже встроен **внутренний In-App Fail2ban** (работает автоматически из коробки в PostgreSQL + in-memory, блокирует перебор на уровне приложения с HTTP 429).

Если вы хотите дополнительно включить блокировку IP на уровне ядра Linux (**iptables / nftables**):

### 1. Скопируйте фильтр и джейл:
```bash
sudo cp deploy/fail2ban/filter.d/pionex-bot.conf /etc/fail2ban/filter.d/
sudo cp deploy/fail2ban/jail.d/pionex-bot.conf /etc/fail2ban/jail.d/
```

### 2. Перезапустите службу fail2ban:
```bash
sudo systemctl restart fail2ban
sudo fail2ban-client status pionex-bot
```

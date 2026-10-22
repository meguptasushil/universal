# Put your app on the internet: a beginner's guide

This guide is for anyone, including people who have never used a terminal. By the end, your
website or app will have a public address that anyone in the world can open.

There are two parts:

| Part | Who does it | How often |
|------|-------------|-----------|
| **A. The relay**: a small server on the internet that gives out public addresses | One person (you, your company's IT, or a friend) | Once |
| **B. The portal**: the app on your computer where you publish things | Everyone who wants to publish | Whenever you like |

If someone already gave you a **connection code** (it starts with `uni1.`), skip to part B.

---

## A. Set up the relay (once, about 5 minutes)

1. **Rent a small cloud server.** Any provider works (DigitalOcean, Hetzner, AWS Lightsail,
   Vultr, Linode, …). Choose the cheapest Linux server (Ubuntu is a good choice). It costs
   about $4–6 a month.
2. **Open the server's console.** Your provider has a "Console" or "SSH" button on the
   server's page.
3. **Paste this line and press Enter:**

   ```sh
   curl -fsSL https://raw.githubusercontent.com/meguptasushil/universal/main/deploy/install.sh | sudo sh -s -- relay
   ```

4. When it finishes, it prints a **connection code**, a long line that starts with `uni1.`.
   Copy it. That's what you give to people who should be able to publish apps.

You don't need to buy a domain or change any DNS settings: the relay automatically gets a
free address like `203-0-113-7.sslip.io`.

> **If your provider has a firewall** (sometimes called a "security group"), allow incoming
> traffic on ports **80** and **443**.
>
> **Want to publish databases or SSH too?** Use this instead, which also opens ports 20000–20100:
> `curl -fsSL …/install.sh | sudo sh -s -- relay "" 20000-20100`
>
> **Want your own domain** (like `myapps.com`)? At your domain registrar, add two DNS
> records, `A myapps.com → server IP` and `A *.myapps.com → server IP`, then install with
> `… | sudo sh -s -- relay myapps.com`.

Lost the code? Run this on the server:
`sudo universal relay --state /var/lib/universal --show-code`

---

## B. Publish from your computer

### 1. Install Universal

- **Windows:** open **PowerShell** (search for it in the Start menu), paste this, and press Enter:

  ```powershell
  irm https://raw.githubusercontent.com/meguptasushil/universal/main/deploy/install.ps1 | iex
  ```

  This adds a **Universal Portal** shortcut to your Desktop.

- **Mac or Linux:** open **Terminal**, paste this, and press Enter:

  ```sh
  curl -fsSL https://raw.githubusercontent.com/meguptasushil/universal/main/deploy/install.sh | sh
  ```

  Then type `universal` and press Enter.

The portal opens in your web browser.

### 2. Connect (first time only)

Paste your connection code and press **Connect**. If there's a problem, the page explains it
in plain words, for example "The relay didn't accept your access code".

### 3. Publish

The portal looks at your computer and lists what's already running, for example
"Corner Shop, port 8000". Press **Publish** next to it.

Or choose what you want to put online:

| Choose | When | You enter |
|--------|------|-----------|
| **An app that's running** | Your app is already open on your computer, for example at `http://localhost:3000` | The port number (`3000`) |
| **A project folder** | You have the project's code but don't know how to start it | The folder (use **Browse…**). Universal recognizes Node.js, Python (Django, Flask, FastAPI, Streamlit), Go and Docker projects and starts them for you |
| **A website folder** | A folder of HTML files | The folder |
| **A database or other service** | PostgreSQL, MySQL, Redis, SSH, a game server… | The port. Also fill in **Who may connect?** with the IP addresses that should have access |

Choose a **public name** (for example `myshop`). The page shows your future address, such as
`https://myshop.203-0-113-7.sslip.io`. Then press **Publish**.

Your app appears under **Your apps** with a green **Online** label and its address. Press
**Copy address** and send it to anyone.

### 4. Everyday use

- **Take offline / Put online**: one click.
- **Logs**: for project folders, shows what your project printed, which helps when something
  goes wrong.
- **Keep it running after a restart**: in **Settings**, tick *Start Universal automatically
  when I log in*. Apps that were online come back by themselves.
- Your computer must stay on (and Universal must stay open) while your apps are online. For
  24/7 hosting, run Universal on an always-on machine or a cloud server.

---

## When something goes wrong

| You see | What to do |
|---------|------------|
| *The relay didn't accept your access code* | Copy the connection code again (all of it) and reconnect in **Settings** |
| *The relay server isn't answering* | Check that the relay server is running, and that ports 80 and 443 are open in its firewall |
| *That public name is already taken* | Choose another public name |
| *Your address works, but your app isn't running* | Start your app on your computer. Visitors see it as soon as it runs |
| *Your project stopped running* | Press **Logs** to see why. Usually a missing program (install Node.js, Python or Docker) or a missing dependency |
| *The relay's HTTPS certificate isn't ready* | Wait a minute after setting up the relay, then try again |

## Safety tips

- Only publish things you mean to share: anyone with the address can open them.
- For databases and SSH, always fill in **Who may connect?** and keep passwords on.
- Treat the connection code like a password. Anyone who has it can publish on your relay.
  To revoke it, delete `/var/lib/universal/relay-token` on the relay server and restart it
  (`sudo systemctl restart universal-relay`). A new code is created; get it with `--show-code`.

# Comment installer FTW

Ce guide est fait pour vous qui n'avez jamais configuré un Raspberry Pi. Pas de panique — c'est plus simple qu'il n'y paraît. Il suffit de suivre les étapes, une par une.

> **Parcours manuel alternatif :** l'installation recommandée utilise l'image FTW prête à l'emploi décrite dans [`../rpi-image.md`](../rpi-image.md). Continuez ici uniquement si vous souhaitez installer vous-même Raspberry Pi OS + Docker.

> **Pas de Raspberry Pi ?** Vous pouvez aussi faire tourner FTW sur un NUC, un vieux portable ou tout autre matériel qui traîne — du moment qu'il peut exécuter Docker. Ce guide se concentre sur la mise en route avec un Raspberry Pi ; si vous êtes sur une autre machine, survolez les étapes matérielles et passez directement à l'**Étape 11 — Installer FTW** (le script d'installation suppose Debian ou Ubuntu).

## Ce dont vous avez besoin

- Un **Raspberry Pi**
- Une **carte mémoire** — incluse si vous avez pris un pack, sinon procurez-vous une microSD séparée
- Un **adaptateur secteur** — inclus si vous avez pris un pack, sinon procurez-vous-en un séparément
- Un **boîtier** — optionnel, sauf si vous avez un RAK wireless ou une Sourceful gateway (ils sont déjà livrés en boîtier)
- **Votre ordinateur habituel** (Windows ou Mac)
- Un **lecteur de carte** sur votre ordinateur habituel (fente intégrée ou petit appareil USB)
- **Internet à la maison** (câble réseau ou Wi-Fi)

---

## Étape 1 — Télécharger le programme qui prépare la carte mémoire

1. Ouvrez le navigateur web sur votre ordinateur habituel.
2. Allez à : **https://www.raspberrypi.com/software/**
3. Cliquez sur **Download** et choisissez la version qui correspond à votre ordinateur (Windows ou Mac).
4. Ouvrez le fichier téléchargé et installez le programme. Cliquez sur "OK" ou "Suivant" jusqu'à ce que ce soit terminé.

## Étape 2 — Lancer le programme

Le programme s'appelle **Raspberry Pi Imager**. Ouvrez-le.

## Étape 3 — Choisir quel Raspberry Pi vous avez

Vous voyez trois grandes cases. Cliquez sur celle du haut et choisissez :

- **Raspberry Pi 4** (c'est presque toujours le bon choix si vous avez un Sourceful gateway ou RAK wireless)

![](images/pi-imager-step1.png)

## Étape 4 — Choisir le logiciel

Cliquez sur la case du milieu ("Operating System") :

- Choisissez **Raspberry Pi OS (other)**
- Puis choisissez **Raspberry Pi OS Lite (64-bit)**

C'est le programme de base qui fait fonctionner le Raspberry Pi — un peu comme Windows sur votre ordinateur habituel.

![](images/pi-imager-step2-select-os.png)

![](images/pi-imager-step3.png)

## Étape 5 — Mettre la carte mémoire dans votre ordinateur

1. Sortez la carte mémoire du Raspberry Pi (si elle est dedans). Manipulez-la doucement.
2. Glissez-la dans le lecteur de carte de votre ordinateur habituel.
3. Cliquez sur la case du bas ("Storage") dans le programme et choisissez votre carte.

## Étape 6 — Personnaliser les réglages

Cliquez sur **Next**. Le programme demande si vous voulez personnaliser les réglages — répondez **Oui** (ou "Edit settings").

Remplissez :

- **Hostname :** `ftw`
- **Localization :** choisissez votre pays
- **Nom d'utilisateur :** p. ex. `pi`
- **Mot de passe :** choisissez-en un que vous retiendrez — mais pas "1234"
- **Wi-Fi :** si vous utilisez le sans fil, tapez le nom de votre Wi-Fi domestique (SSID) et le mot de passe. Vous utilisez un câble réseau ? Sautez cette étape.
- **Remote access :** activez **SSH**. Si vous êtes un utilisateur avancé — choisissez la connexion par **clé SSH** (collez votre clé publique). Sinon, choisissez **mot de passe + nom d'utilisateur**.
- **Raspberry Pi Connect :** cliquez simplement pour passer.

## Étape 7 — Écrire sur la carte

Cliquez sur le bouton qui écrit tout sur la carte mémoire. Attendez que ce soit terminé. Éjectez la carte quand c'est fini.

## Étape 8 — Mettre la carte dans le Raspberry Pi

1. L'adaptateur secteur doit être **DÉBRANCHÉ** du Raspberry Pi.
2. Insérez la carte mémoire.
3. Branchez l'adaptateur secteur.
4. Attendez environ **10 minutes**. La lumière clignote puis reste allumée. C'est normal. Allez vous faire un thé.

## Étape 9 — Trouver l'adresse IP du Raspberry Pi

Votre Raspberry Pi a maintenant une **adresse IP** sur votre réseau domestique.

Connectez-vous à votre box internet. Regardez dans "appareils connectés". Cherchez quelque chose appelé **ftw**.

Notez l'adresse IP. Elle ressemble à : **192.168.1.xxx**.

> Vous ne savez pas comment vous connecter à la box ? Demandez à quelqu'un qui s'y connaît, ou essayez l'adresse web **192.168.1.1** dans le navigateur.

## Étape 10 — Parler au Raspberry Pi

### Sous Windows

1. Téléchargez **PuTTY** depuis le site officiel : **https://www.chiark.greenend.org.uk/~sgtatham/putty/latest.html**
2. Installez le programme et lancez-le.
3. Dans la case "Host Name" : tapez l'adresse IP que vous avez notée.
4. Cliquez sur **Open**. Si un avertissement apparaît — cliquez sur **Yes / Accept**.
5. Tapez le nom d'utilisateur que vous avez choisi.
6. Tapez le mot de passe (vous ne verrez aucun point en tapant — c'est fait exprès).

### Sous Mac

1. Ouvrez le **Terminal** (appuyez sur `Cmd`+espace, tapez "Terminal", entrée).
2. Tapez (remplacez l'adresse IP et le nom d'utilisateur par les vôtres) :

   ```
   ssh pi@192.168.1.123
   ```

3. Appuyez sur entrée. S'il dit "Are you sure?" — tapez **yes** et appuyez sur entrée.
4. Tapez votre mot de passe (vous ne voyez aucun caractère en tapant). Appuyez sur entrée.

Bravo — vous êtes maintenant "à l'intérieur" du Raspberry Pi.

## Étape 11 — Installer FTW

Copiez cette ligne EXACTEMENT telle qu'elle est :

```
curl -fsSL https://raw.githubusercontent.com/srcfl/ftw/master/scripts/install.sh | bash
```

Collez-la dans le terminal ou PuTTY (le clic droit colle généralement) et appuyez sur entrée.

Tapez votre mot de passe une dernière fois. Tout s'installe maintenant. Cela prend quelques minutes. **Et c'est fini.**

## Terminé

Ouvrez le navigateur sur votre ordinateur habituel et allez à l'interface web :

```
http://ftw.local:8080/
```

Si cette adresse ne fonctionne pas — essayez l'adresse IP que vous avez notée, p. ex. `http://192.168.1.123:8080/`.

---

## Si quelque chose ne fonctionne pas

- **La lumière ne s'allume pas du tout** → vérifiez que l'adaptateur secteur est bien branché.
- **Pas d'adresse IP trouvée** → redémarrez la box, attendez 5 minutes, regardez à nouveau.
- **SSH dit "Connection refused"** → attendez encore un peu. Le premier démarrage prend du temps.
- **Rien de tout cela ne marche** → passez sur notre Discord et demandez gentiment de l'aide : **https://discord.gg/25xcBzQaux**


# Tunnel TCP

Un tunnel TCP simple et efficace conçu pour fonctionner derrière un reverse proxy. Ce projet fournit un moyen de relayer le trafic TCP d'un serveur public vers un service local, même lorsque le service local se trouve derrière un NAT ou un pare-feu.

## ✨ Fonctionnalités

- **Tunneling TCP** : Relaye les connexions TCP d'un point d'accès public vers un service sur un réseau local.
- **Architecture Client-Serveur** : Un serveur central gère les connexions publiques et un client léger s'exécute sur le réseau local.
- **Multiplexage de Connexions** : Utilise une connexion de contrôle persistante pour gérer plusieurs connexions de tunnel, ce qui le rend compatible avec les reverse proxies.
- **Gestion des Connexions** : Suit les connexions actives et nettoie les connexions obsolètes ou en attente.
- **Reconnexion Automatique** : Le client tente de se reconnecter automatiquement au serveur en cas de déconnexion.
- **Journalisation Configurable** : Niveaux de log ajustables (`debug`, `info`, `warn`, `error`) pour le développement et la production.
- **Détection de Health Check** : Le serveur peut répondre aux requêtes HTTP GET de base (comme les health checks des reverse proxies) sans interrompre la connexion de tunnel.

## ⚙️ Comment ça marche

L'application se compose de deux parties : un **serveur** et un **client**.

### Le Client

- S'exécute sur la même machine ou le même réseau que le service que vous souhaitez exposer (ex: un serveur web local sur le port 3000).
- Établit une connexion de contrôle persistante avec le **serveur**.

### Le Serveur

- S'exécute sur une machine accessible publiquement (ex: un VPS).
- Il écoute sur deux ports :
  - **Port de contrôle** (par défaut `8080`) pour communiquer avec le client.
  - **Port de service public** (défini par l'utilisateur) pour recevoir le trafic des utilisateurs finaux.

### Flux de Connexion

1. Lorsqu'un utilisateur se connecte au port de service du serveur, celui-ci **n'accepte pas directement** la connexion.
2. Il notifie le client via la connexion de contrôle.
3. Le client ouvre une **nouvelle connexion** vers le serveur.
4. Le client se connecte localement au service cible (ex: `localhost:3000`).
5. Le serveur relie la connexion utilisateur à celle du client, qui la relie ensuite au service local.
6. Le trafic TCP peut désormais circuler de bout en bout à travers le tunnel.

Ce mécanisme permet de contourner les limitations des reverse proxies qui ne gèrent que HTTP/HTTPS et ne maintiennent pas de connexions TCP longues.

## 🛠️ Installation

Vous devez avoir **Go** installé sur votre machine.

```sh
git clone <url-du-repo>
cd <nom-du-repo>
go build -o tunnel .
```

## 🚀 Utilisation

Le binaire `tunnel` peut fonctionner en mode **serveur** ou **client**.

### Côté Serveur

Exécutez la commande suivante sur votre serveur public :

```sh
./tunnel server [service_port] --log-level [level]
```

- `[service_port]` : **(Obligatoire)** Le port TCP public qui recevra le trafic à tunneler.
- `--log-level` : **(Optionnel)** Niveau de verbosité des logs (`debug`, `info`, `warn`, `error`). Défaut : `info`.

#### Exemple :

```sh
./tunnel server 80 --log-level debug
```

Le serveur écoutera également les connexions de contrôle sur le port **8080**.

### Côté Client

Exécutez la commande suivante sur la machine hébergeant le service local :

```sh
./tunnel client [local_port] [server_address] --log-level [level]
```

- `[local_port]` : **(Obligatoire)** Port local du service à exposer (ex: 3000).
- `[server_address]` : **(Obligatoire)** Adresse IP ou nom de domaine du serveur public.
- `--log-level` : **(Optionnel)** Niveau de log.

#### Exemple :

```sh
./tunnel client 3000 example.com
```

Une fois connecté, tout le trafic envoyé à l’adresse publique sera redirigé vers `localhost:3000`.

En cas de déconnexion, le client tente automatiquement de se reconnecter toutes les 5 secondes.

## 🧪 Health Check

Le serveur peut répondre à une requête `GET /` sur le port de contrôle (8080) avec un `200 OK`, ce qui permet aux reverse proxies ou load balancers de vérifier sa disponibilité.

## 📄 Licence

Ce projet est open-source. Licence à définir selon vos besoins.

## 🤝 Contribuer

Les contributions sont les bienvenues ! N'hésitez pas à ouvrir une issue ou une pull request.

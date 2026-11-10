# Crédits par conversation avec Codex

## Périmètre

Le mode `backsight mcp --client codex` expose la recherche, la lecture et les crédits `used` ou `none`. Il n'expose pas l'enregistrement de faits et refuse les corrections. Il ne lance aucun hook, ne lit aucun transcript et n'archive pas les conversations. Le mode par défaut reste celui du plugin Claude Code.

Un même binaire peut servir les deux clients, avec une entrée MCP par client. Le plugin Claude Code conserve sa commande par défaut. Pour les essais, utiliser des répertoires et une banque de test indépendants. Le serveur doit recevoir la configuration et les secrets par les mécanismes Backsight habituels ; aucun secret dans les arguments MCP.

## Identité et journalisation

Chaque requête doit contenir dans `_meta` les champs `threadId`, `callId` et l'objet `x-codex-turn-metadata` avec `turn_id`. Si `thread_id` est également présent dans cet objet, il doit correspondre à `threadId`. Ces champs proviennent du transport du client, pas des arguments de l'outil. Ils ne constituent pas une authentification pour un serveur exposé à des utilisateurs non fiables : ce mode est destiné au transport stdio local.

Les sessions du journal sont préfixées par `codex-`. Aucune association par PID ou session Claude n'est créée. Une réponse de recherche ou de lecture réussie journalise ses identifiants avant son retour ; une annulation ultérieure du transport ne prouve pas que le modèle l'a reçue. Un crédit n'est accepté que pour des souvenirs déjà journalisés dans la même conversation, y compris lors d'un tour précédent. Les forks ont des identifiants distincts et doivent rechercher à nouveau leurs souvenirs.

Un verrou par conversation protège l'attribution ; deux appels simultanés dans la même conversation peuvent recevoir une erreur temporaire à réessayer. Un appel sans identité complète, une session en pause, une erreur de lecture ou d'écriture du journal ne produisent pas un faux succès.

Un tour accepte un verdict. Répéter le même verdict avec les mêmes identifiants, même après reconnexion, ne crée aucun crédit supplémentaire. Un verdict différent dans ce tour est refusé. Les identifiants d'appels de recherche/lecture déjà journalisés sont refusés au rejeu : un nouvel appel doit avoir son propre identifiant. Le journal contient des IDs, rangs et provenance, jamais le texte des souvenirs.

Les crédits utilisent le journal et le calcul de statistiques existants. Leur consolidation suit les délais habituels de Backsight : le succès de l'outil signifie que le verdict est journalisé, pas qu'une page de statistiques s'est déjà rafraîchie. Sans hook de fin de tour, le calcul peut attendre le tour suivant ou l'expiration prévue par le moteur.

## Installation et vérification

Dans la configuration MCP du client, sélectionner le binaire Backsight et les arguments `["mcp", "--client", "codex"]`. Recharger la connexion. Les hooks ne sont pas nécessaires pour ce périmètre.

`memory_credit` écrit dans le journal local : il ne porte donc pas l'annotation de lecture seule. Le client peut demander une autorisation pour cet outil. En mode non interactif avec une politique qui interdit les demandes, l'appel échoue si l'outil n'a pas été autorisé au préalable. Conserver les règles d'approbation du client ; ne pas déclarer cet outil en lecture seule pour éviter cette demande. Les options d'autorisation par outil sont décrites dans la [documentation MCP du client](https://learn.chatgpt.com/docs/extend/mcp).

Valider dans une vraie conversation : rechercher un souvenir, lire son ID, appeler `memory_credit` avec cet ID, puis vérifier une seule ligne `credit` sous la session `codex-<threadId>`. Une deuxième conversation ne doit pas pouvoir créditer cet ID sans l'avoir recherché ou lu elle-même. Un nouvel appel avec `verdict: "none"` est prévu dans un nouveau tour, sans IDs.

L'inférence du projet de recherche reste celle du répertoire de lancement du serveur ; pour une connexion partagée entre plusieurs projets, fournir les projets concernés ou utiliser `scope: "all"`. Ce changement ne réalise pas l'adaptation complète des worktrees et des événements de session.

## Retour arrière

Retirer uniquement l'entrée MCP du nouveau client, puis recharger sa connexion. Conserver les journaux pour le diagnostic. Cette opération ne supprime aucune mémoire et ne touche pas au plugin Claude Code.

## Contrôles de développement

Les tests `TestCodex*` couvrent l'association, le refus des crédits étrangers, les reconnexions, les répétitions, les métadonnées manquantes ou contradictoires, les erreurs de journal, les pauses, les appels concurrents et l'intégration au calcul des statistiques. Les tests MCP existants vérifient le comportement du client par défaut.

Un essai de transport avec le client 0.167.0, un serveur de réponses déterministes et une banque simulée vérifie également la lecture et un crédit via le véritable transport stdio. Aucun modèle payant ni banque personnelle n'est nécessaire pour cet essai. Cela ne valide pas les décisions d'un modèle réel sur les souvenirs à utiliser.

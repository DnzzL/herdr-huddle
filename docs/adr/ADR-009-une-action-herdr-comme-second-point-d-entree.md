# ADR-009 — une action Herdr comme second point d'entrée

- Statut : accepté
- Date : 2026-10-02
- Responsable : Thomas Legrand
- Prolonge ADR-002 (ce à quoi un partage est lié) et ADR-007 (le projet est la
  racine de worktree)

## Contexte

`share` apprenait sur quoi il agit par l'ambiance : le dépôt venait de
`os.Getwd()`, le pane de `HERDR_PANE_ID`. Rien ne rapprochait les deux, si bien
qu'une commande lancée depuis le pane d'un projet pouvait ouvrir une pull
request sur un autre. Un garde-fou ajouté à ADR-002 refuse désormais ce cas,
mais il traite le symptôme : il dit non là où la question est « d'où la commande
tient-elle sa cible ».

Une action Herdr pose la question autrement, parce qu'elle fournit la réponse.
Mesuré sur 0.9.0 avec un plugin jetable, invoqué par
`herdr plugin action invoke` :

- **Le répertoire courant d'une action est la racine du plugin**, jamais celle
  du projet. `os.Getwd()` n'est donc pas seulement fragile dans ce chemin : il
  est systématiquement faux.
- **`HERDR_PANE_ID` est fourni**, et vaut le pane ayant le focus.
- **`HERDR_PLUGIN_CONTEXT_JSON` porte** `workspace_id`, `workspace_label`,
  `workspace_cwd`, `tab_id`, `tab_label`, `focused_pane_id`,
  `focused_pane_cwd`, `focused_pane_agent`, `focused_pane_status`,
  `invocation_source` et `correlation_id`. **Pas de racine de worktree**, et pas
  de texte sélectionné, contrairement à ce qu'annonce la documentation : il
  faut toujours interroger git.
- **La sortie standard d'une action ne va qu'au journal** (`herdr plugin log`).
  L'opérateur ne la voit pas.
- **Herdr n'attend pas une action** : l'appel rend la main immédiatement, statut
  « running », et l'action va jusqu'au bout en arrière-plan.

## Décision

1. **Une commande demande sa cible, elle ne la devine plus.** `target.Resolve`
   rend un `Target{PaneID, Root, Agent}` : la racine de worktree de l'agent
   quand un pane est connu, le répertoire courant sinon. `share` ouvre le fil
   sur `Target.Root`.

   Les deux sources ne peuvent alors plus se contredire, puisque l'appelant n'en
   voit qu'une. Le garde-fou d'ADR-002 reste en place : il ne peut plus se
   déclencher par le chemin normal, et c'est précisément ce qui en fait une
   assertion utile si `Resolve` régresse un jour.

2. **`serve --notify` annonce le lien de jointure par une notification Herdr.**
   La sortie d'une action n'atteignant personne, un huddle ouvert par une action
   serait un tunnel dont l'URL dort dans un journal. La notification passe par
   la même porte que le reste (ADR-008) ; le drapeau existe pour que l'action
   n'ait pas à lire la sortie du serveur.

3. **`serve` sert le pane d'où il a été lancé.** L'action s'intitule « sur ce
   pane » et Herdr lui donne celui qui a le focus ; sans cela elle diffuserait
   le partage actif du moment, c'est-à-dire l'agent de quelqu'un d'autre, et
   par un tunnel public. `--pane` l'emporte toujours. Être dans un pane auquel
   aucun partage n'est lié est un refus, pas un repli : diffuser un autre pane
   en silence est la liaison erronée que `share` vient d'apprendre à refuser.
   Hors de tout pane, l'unique partage actif reste la réponse.

4. **Deux actions, pas davantage** : ouvrir le fil sur ce projet, ouvrir le
   huddle sur ce pane. `join` n'en sera pas une — il lui faut un terminal, et
   une action n'en a pas.

## Alternatives rejetées

- **Un script d'enveloppe qui lit `HERDR_PLUGIN_CONTEXT_JSON` et fait `cd`.**
  Aucune modification Go, et c'est ce qui l'a rendu tentant. Rejeté : extraire
  un champ JSON en `sh` demande `jq` — une dépendance de plus — ou un `sed` sur
  du JSON, qui est faux dès qu'un chemin contient un guillemet.
- **Dériver le dépôt du pane partout, et supprimer le garde-fou.** C'est ce que
  fait `Resolve` ; garder le garde-fou coûte quelques lignes et transforme une
  régression silencieuse en échec bruyant.
- **Un gestionnaire de liens dès maintenant.** `[[link_handlers]]` associe une
  regex d'URL à une action et lui passe `clicked_url`. Séduisant pour ouvrir le
  huddle d'une PR depuis son URL, mais il faudrait d'abord une façon d'aller de
  l'URL au partage. Reporté, faute d'usage établi.
- **Une interface de rapport côté Go** pour router la sortie selon l'appelant.
  Une seule commande a quelque chose à annoncer ; un drapeau suffit.

## Conséquences

- `share` lancé depuis n'importe quel répertoire, pane connu, ouvre le fil sur
  le projet de l'agent. C'est un changement de comportement pour qui lançait la
  commande ailleurs en comptant sur le répertoire courant.
- Le plugin gagne deux points d'entrée que personne ne supervise. Une action
  `serve` est un serveur au premier plan sous un autre nom : Herdr l'enregistre
  « running » et ne la redémarre pas — le manque d'ADR-005 reste entier, et
  s'affiche désormais dans l'interface.

## Known gaps

- not tested : les actions n'ont été invoquées que par `herdr plugin action
  invoke`. Le chemin clavier et la valeur de `focused_pane_id` quand le focus
  change entre l'appui et l'exécution n'ont pas été mesurés.
- not done : rien ne relie une URL de pull request à son partage, donc pas de
  gestionnaire de liens.
- unknown : ce que devient une action `serve` quand le serveur Herdr redémarre.
  Le journal la dit « running » ; personne ne vérifie qu'elle l'est encore.

package web

const dashboardTemplate = `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Refity Docker Registry Dashboard</title>
    <link href="https://cdn.jsdelivr.net/npm/bootstrap@5.3.7/dist/css/bootstrap.min.css" rel="stylesheet">
</head>
<body class="bg-light">
    <div class="container-fluid" x-data="dashboard()" x-init="init()">
        <!-- Header -->
        <nav class="navbar navbar-expand-lg navbar-dark bg-primary mb-4">
            <div class="container-fluid">
                <span class="navbar-brand">Refity Docker Registry</span>
                <div class="navbar-nav ms-auto">
                    <button @click="refreshData()" class="btn btn-outline-light">
                        <i class="bi bi-arrow-clockwise"></i> Refresh
                    </button>
                </div>
            </div>
        </nav>

        <div class="container-fluid">
            <!-- Statistics Cards -->
            <div class="row mb-4">
                <div class="col-md-4">
                    <div class="card">
                        <div class="card-body">
                            <div class="d-flex align-items-center">
                                <div class="flex-shrink-0">
                                    <i class="bi bi-images fs-1 text-primary"></i>
                                </div>
                                <div class="flex-grow-1 ms-3">
                                    <h6 class="card-subtitle mb-1 text-muted">Total Images</h6>
                                    <h4 class="card-title mb-0">{{.TotalImages}}</h4>
                                </div>
                            </div>
                        </div>
                    </div>
                </div>

                <div class="col-md-4">
                    <div class="card">
                        <div class="card-body">
                            <div class="d-flex align-items-center">
                                <div class="flex-shrink-0">
                                    <i class="bi bi-folder fs-1 text-success"></i>
                                </div>
                                <div class="flex-grow-1 ms-3">
                                    <h6 class="card-subtitle mb-1 text-muted">Total Repositories</h6>
                                    <h4 class="card-title mb-0">{{len .Repositories}}</h4>
                                </div>
                            </div>
                        </div>
                    </div>
                </div>

                <div class="col-md-4">
                    <div class="card">
                        <div class="card-body">
                            <div class="d-flex align-items-center">
                                <div class="flex-shrink-0">
                                    <i class="bi bi-hdd fs-1 text-warning"></i>
                                </div>
                                <div class="flex-grow-1 ms-3">
                                    <h6 class="card-subtitle mb-1 text-muted">Total Size</h6>
                                    <h4 class="card-title mb-0">{{formatBytes .TotalSize}}</h4>
                                </div>
                            </div>
                        </div>
                    </div>
                </div>
            </div>

            <!-- Create Repository Button -->
            <div class="mb-4">
                <button @click="showCreateModal()" 
                        class="btn btn-primary">
                    <i class="bi bi-plus-circle"></i> Create New Repository
                </button>
            </div>

            <!-- Repositories List -->
            <div class="card">
                <div class="card-header">
                    <h5 class="card-title mb-0">Repositories</h5>
                    <small class="text-muted">List of all repositories and their tags</small>
                </div>
                <div class="card-body p-0">
                    {{if .Repositories}}
                        <div class="list-group list-group-flush">
                            {{range .Repositories}}
                            {{$repoName := .Name}}
                            <div class="list-group-item">
                                <div class="d-flex justify-content-between align-items-start">
                                    <div class="flex-grow-1">
                                        <h6 class="mb-2">{{.Name}}</h6>
                                        <div class="d-flex flex-wrap gap-1">
                                            {{if .Tags}}
                                                {{range .Tags}}
                                                <span class="badge bg-primary d-inline-flex align-items-center">
                                                    {{.Name}} ({{formatBytes .Size}})
                                                    <button @click="deleteTag('{{$repoName}}', '{{.Name}}')" 
                                                            class="btn-close btn-close-white ms-2" 
                                                            style="font-size: 0.5rem;"></button>
                                                </span>
                                                {{end}}
                                            {{else}}
                                                <span class="badge bg-secondary">No images yet</span>
                                            {{end}}
                                        </div>
                                    </div>
                                    <div class="ms-3">
                                        <button @click="deleteRepository('{{.Name}}')" 
                                                class="btn btn-outline-danger btn-sm">
                                            <i class="bi bi-trash"></i> Delete
                                        </button>
                                    </div>
                                </div>
                            </div>
                            {{end}}
                        </div>
                    {{else}}
                        <div class="text-center py-5">
                            <i class="bi bi-folder-x display-1 text-muted"></i>
                            <h5 class="mt-3">No repositories</h5>
                            <p class="text-muted">Create a repository to get started.</p>
                        </div>
                    {{end}}
                </div>
            </div>
        </div>
    </div>

    <!-- Create Repository Modal -->
    <div class="modal fade" id="createModal" tabindex="-1" aria-labelledby="createModalLabel" aria-hidden="true">
        <div class="modal-dialog">
            <div class="modal-content">
                <div class="modal-header">
                    <h5 class="modal-title" id="createModalLabel">
                        <i class="bi bi-plus-circle text-primary"></i> Create New Repository
                    </h5>
                    <button type="button" class="btn-close" data-bs-dismiss="modal" aria-label="Close"></button>
                </div>
                <div class="modal-body">
                    <div class="mb-3">
                        <label for="repo-name" class="form-label">Repository Name</label>
                        <input type="text" 
                               id="repo-name"
                               x-model="newRepoName" 
                               placeholder="Enter repository name (e.g., myapp)" 
                               class="form-control"
                               @keyup.enter="createRepository()">
                        <div x-show="createMessage" 
                             x-text="createMessage" 
                             :class="createSuccess ? 'text-success' : 'text-danger'" 
                             class="form-text mt-2"></div>
                    </div>
                </div>
                <div class="modal-footer">
                    <button type="button" class="btn btn-secondary" data-bs-dismiss="modal" @click="hideCreateModal()">
                        Cancel
                    </button>
                    <button type="button" class="btn btn-primary" @click="createRepository()">
                        Create Repository
                    </button>
                </div>
            </div>
        </div>
    </div>

    <script src="https://cdn.jsdelivr.net/npm/bootstrap@5.3.7/dist/js/bootstrap.bundle.min.js"></script>
    <script src="https://cdn.jsdelivr.net/npm/alpinejs@3.x.x/dist/cdn.min.js" defer></script>
    <script>
        function dashboard() {
            return {
                newRepoName: '',
                createMessage: '',
                createSuccess: false,
                credentials: null,
                modal: null,
                
                init() {
                    // Initialize Bootstrap modal
                    this.modal = new bootstrap.Modal(document.getElementById('createModal'));
                },
                
                showCreateModal() {
                    this.modal.show();
                },
                
                hideCreateModal() {
                    this.modal.hide();
                    this.newRepoName = '';
                    this.createMessage = '';
                },
                
                async createRepository() {
                    if (!this.newRepoName.trim()) {
                        this.createMessage = 'Repository name is required';
                        this.createSuccess = false;
                        return;
                    }

                    // Get credentials if not already set
                    if (!this.credentials) {
                        const username = prompt('Enter username:');
                        const password = prompt('Enter password:');
                        if (!username || !password) {
                            this.createMessage = 'Authentication required';
                            this.createSuccess = false;
                            return;
                        }
                        this.credentials = btoa(username + ':' + password);
                    }

                    try {
                        const response = await fetch('/api/repositories', {
                            method: 'POST',
                            headers: {
                                'Content-Type': 'application/json',
                                'Authorization': 'Basic ' + this.credentials
                            },
                            body: JSON.stringify({ name: this.newRepoName.trim() })
                        });

                        const result = await response.json();
                        
                        if (response.ok) {
                            this.createMessage = result.message;
                            this.createSuccess = true;
                            this.newRepoName = '';
                            setTimeout(() => {
                                this.createMessage = '';
                                this.hideCreateModal();
                                window.location.reload();
                            }, 2000);
                        } else {
                            this.createMessage = result.message || 'Failed to create repository';
                            this.createSuccess = false;
                        }
                    } catch (error) {
                        this.createMessage = 'Error creating repository';
                        this.createSuccess = false;
                    }
                },

                async deleteRepository(repoName) {
                    if (!confirm('Are you sure you want to delete repository "' + repoName + '" and all its tags?')) {
                        return;
                    }

                    // Get credentials if not already set
                    if (!this.credentials) {
                        const username = prompt('Enter username:');
                        const password = prompt('Enter password:');
                        if (!username || !password) {
                            alert('Authentication required');
                            return;
                        }
                        this.credentials = btoa(username + ':' + password);
                    }

                    try {
                        const response = await fetch('/api/repositories/' + encodeURIComponent(repoName), {
                            method: 'DELETE',
                            headers: {
                                'Authorization': 'Basic ' + this.credentials
                            }
                        });

                        if (response.ok) {
                            window.location.reload();
                        } else {
                            const result = await response.json();
                            alert('Failed to delete repository: ' + (result.message || 'Unknown error'));
                        }
                    } catch (error) {
                        alert('Error deleting repository');
                    }
                },

                async deleteTag(repoName, tagName) {
                    if (!confirm('Are you sure you want to delete tag "' + tagName + '" from repository "' + repoName + '"?')) {
                        return;
                    }

                    // Get credentials if not already set
                    if (!this.credentials) {
                        const username = prompt('Enter username:');
                        const password = prompt('Enter password:');
                        if (!username || !password) {
                            alert('Authentication required');
                            return;
                        }
                        this.credentials = btoa(username + ':' + password);
                        return;
                    }

                    try {
                        const response = await fetch('/api/repositories/' + encodeURIComponent(repoName) + '/tags/' + encodeURIComponent(tagName), {
                            method: 'DELETE',
                            headers: {
                                'Authorization': 'Basic ' + this.credentials
                            }
                        });

                        if (response.ok) {
                            window.location.reload();
                        } else {
                            const result = await response.json();
                            alert('Failed to delete tag: ' + (result.message || 'Unknown error'));
                        }
                    } catch (error) {
                        alert('Error deleting tag');
                    }
                },

                refreshData() {
                    window.location.reload();
                }
            }
        }
    </script>
</body>
</html>
`

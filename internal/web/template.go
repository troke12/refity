package web

const dashboardTemplate = `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Refity Docker Registry Dashboard</title>
    <link href="https://cdn.jsdelivr.net/npm/bootstrap@5.3.7/dist/css/bootstrap.min.css" rel="stylesheet">
    <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/bootstrap-icons@1.11.3/font/bootstrap-icons.min.css">
    <style>
        :root {
            --primary-gradient: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            --success-gradient: linear-gradient(135deg, #11998e 0%, #38ef7d 100%);
            --warning-gradient: linear-gradient(135deg, #f093fb 0%, #f5576c 100%);
            --info-gradient: linear-gradient(135deg, #4facfe 0%, #00f2fe 100%);
            --shadow-sm: 0 2px 4px rgba(0,0,0,0.1);
            --shadow-md: 0 4px 6px rgba(0,0,0,0.1);
            --shadow-lg: 0 10px 25px rgba(0,0,0,0.15);
            --shadow-xl: 0 20px 40px rgba(0,0,0,0.2);
        }

        body {
            background: linear-gradient(135deg, #f5f7fa 0%, #c3cfe2 100%);
            min-height: 100vh;
            font-family: 'Segoe UI', Tahoma, Geneva, Verdana, sans-serif;
        }

        .navbar {
            background: var(--primary-gradient) !important;
            box-shadow: var(--shadow-md);
            padding: 1rem 0;
        }

        .navbar-brand {
            font-weight: 700;
            font-size: 1.5rem;
            letter-spacing: -0.5px;
        }

        .stat-card {
            border: none;
            border-radius: 16px;
            box-shadow: var(--shadow-md);
            transition: all 0.3s ease;
            overflow: hidden;
            position: relative;
            background: white;
        }

        .stat-card::before {
            content: '';
            position: absolute;
            top: 0;
            left: 0;
            right: 0;
            height: 4px;
            background: var(--primary-gradient);
            transform: scaleX(0);
            transition: transform 0.3s ease;
        }

        .stat-card:hover {
            transform: translateY(-5px);
            box-shadow: var(--shadow-lg);
        }

        .stat-card:hover::before {
            transform: scaleX(1);
        }

        .stat-card.primary::before {
            background: var(--primary-gradient);
        }

        .stat-card.success::before {
            background: var(--success-gradient);
        }

        .stat-card.warning::before {
            background: var(--warning-gradient);
        }

        .stat-icon {
            width: 70px;
            height: 70px;
            border-radius: 12px;
            display: flex;
            align-items: center;
            justify-content: center;
            font-size: 2rem;
            background: var(--primary-gradient);
            color: white;
            box-shadow: var(--shadow-sm);
        }

        .stat-card.success .stat-icon {
            background: var(--success-gradient);
        }

        .stat-card.warning .stat-icon {
            background: var(--warning-gradient);
        }

        .stat-value {
            font-size: 2rem;
            font-weight: 700;
            color: #2d3748;
            margin: 0;
        }

        .stat-label {
            font-size: 0.875rem;
            color: #718096;
            font-weight: 500;
            text-transform: uppercase;
            letter-spacing: 0.5px;
        }

        .btn-primary {
            background: var(--primary-gradient);
            border: none;
            border-radius: 10px;
            padding: 0.75rem 1.5rem;
            font-weight: 600;
            box-shadow: var(--shadow-sm);
            transition: all 0.3s ease;
        }

        .btn-primary:hover {
            transform: translateY(-2px);
            box-shadow: var(--shadow-md);
            background: var(--primary-gradient);
        }

        .btn-outline-light {
            border-radius: 10px;
            padding: 0.5rem 1rem;
            font-weight: 500;
            transition: all 0.3s ease;
        }

        .btn-outline-light:hover {
            transform: translateY(-2px);
            box-shadow: var(--shadow-sm);
        }

        .repo-card {
            border: none;
            border-radius: 16px;
            box-shadow: var(--shadow-md);
            overflow: hidden;
            background: white;
        }

        .repo-card-header {
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            color: white;
            padding: 1.5rem;
            border: none;
        }

        .repo-card-header h5 {
            font-weight: 700;
            margin: 0;
            font-size: 1.25rem;
        }

        .repo-item {
            padding: 1.5rem;
            border-bottom: 1px solid #e2e8f0;
            transition: all 0.2s ease;
            background: white;
        }

        .repo-item:last-child {
            border-bottom: none;
        }

        .repo-item:hover {
            background: #f7fafc;
            padding-left: 2rem;
        }

        .repo-name {
            font-size: 1.1rem;
            font-weight: 600;
            color: #2d3748;
            margin-bottom: 0.75rem;
            display: flex;
            align-items: center;
            gap: 0.5rem;
        }

        .repo-name i {
            color: #667eea;
        }

        .tag-badge {
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            color: white;
            padding: 0.5rem 1rem;
            border-radius: 20px;
            font-size: 0.875rem;
            font-weight: 500;
            display: inline-flex;
            align-items: center;
            gap: 0.5rem;
            margin: 0.25rem;
            box-shadow: var(--shadow-sm);
            transition: all 0.2s ease;
        }

        .tag-badge:hover {
            transform: scale(1.05);
            box-shadow: var(--shadow-md);
        }

        .tag-close {
            background: rgba(255,255,255,0.3);
            border-radius: 50%;
            width: 18px;
            height: 18px;
            display: flex;
            align-items: center;
            justify-content: center;
            cursor: pointer;
            transition: all 0.2s ease;
            font-size: 0.7rem;
            padding: 0;
        }

        .tag-close:hover {
            background: rgba(255,255,255,0.5);
            transform: scale(1.1);
        }

        .btn-delete {
            border-radius: 8px;
            padding: 0.5rem 1rem;
            font-weight: 500;
            transition: all 0.2s ease;
        }

        .btn-delete:hover {
            transform: translateY(-2px);
            box-shadow: var(--shadow-sm);
        }

        .empty-state {
            padding: 4rem 2rem;
            text-align: center;
        }

        .empty-state i {
            font-size: 5rem;
            color: #cbd5e0;
            margin-bottom: 1rem;
        }

        .empty-state h5 {
            color: #4a5568;
            font-weight: 600;
            margin-bottom: 0.5rem;
        }

        .empty-state p {
            color: #718096;
        }

        .modal-content {
            border: none;
            border-radius: 16px;
            box-shadow: var(--shadow-xl);
        }

        .modal-header {
            background: var(--primary-gradient);
            color: white;
            border-radius: 16px 16px 0 0;
            border: none;
            padding: 1.5rem;
        }

        .modal-header .btn-close {
            filter: invert(1);
        }

        .modal-title {
            font-weight: 700;
            font-size: 1.25rem;
        }

        .form-control {
            border-radius: 10px;
            border: 2px solid #e2e8f0;
            padding: 0.75rem 1rem;
            transition: all 0.2s ease;
        }

        .form-control:focus {
            border-color: #667eea;
            box-shadow: 0 0 0 3px rgba(102, 126, 234, 0.1);
        }

        .form-label {
            font-weight: 600;
            color: #4a5568;
            margin-bottom: 0.5rem;
        }

        .modal-footer {
            border-top: 1px solid #e2e8f0;
            padding: 1.5rem;
        }

        .btn-secondary {
            border-radius: 10px;
            padding: 0.75rem 1.5rem;
            font-weight: 500;
        }

        .loading {
            display: inline-block;
            width: 1rem;
            height: 1rem;
            border: 2px solid rgba(255,255,255,0.3);
            border-radius: 50%;
            border-top-color: white;
            animation: spin 0.6s linear infinite;
        }

        @keyframes spin {
            to { transform: rotate(360deg); }
        }

        .fade-in {
            animation: fadeIn 0.3s ease-in;
        }

        @keyframes fadeIn {
            from { opacity: 0; transform: translateY(10px); }
            to { opacity: 1; transform: translateY(0); }
        }

        .container-main {
            max-width: 1400px;
            margin: 0 auto;
            padding: 2rem;
        }

        @media (max-width: 768px) {
            .container-main {
                padding: 1rem;
            }
            
            .stat-card {
                margin-bottom: 1rem;
            }
        }
    </style>
</head>
<body>
    <div class="container-main" x-data="dashboard()" x-init="init()">
        <!-- Header -->
        <nav class="navbar navbar-expand-lg navbar-dark mb-4">
            <div class="container-fluid">
                <span class="navbar-brand">
                    <i class="bi bi-box-seam me-2"></i>Refity Docker Registry
                </span>
                <div class="navbar-nav ms-auto">
                    <button @click="refreshData()" 
                            class="btn btn-outline-light"
                            :disabled="isRefreshing">
                        <span x-show="!isRefreshing">
                            <i class="bi bi-arrow-clockwise"></i> Refresh
                        </span>
                        <span x-show="isRefreshing" class="loading"></span>
                    </button>
                </div>
            </div>
        </nav>

        <div class="container-fluid px-0">
            <!-- Statistics Cards -->
            <div class="row mb-4 g-3">
                <div class="col-md-4">
                    <div class="stat-card primary fade-in">
                        <div class="card-body p-4">
                            <div class="d-flex align-items-center">
                                <div class="stat-icon me-3">
                                    <i class="bi bi-images"></i>
                                </div>
                                <div class="flex-grow-1">
                                    <p class="stat-label mb-1">Total Images</p>
                                    <h2 class="stat-value mb-0">{{.TotalImages}}</h2>
                                </div>
                            </div>
                        </div>
                    </div>
                </div>

                <div class="col-md-4">
                    <div class="stat-card success fade-in">
                        <div class="card-body p-4">
                            <div class="d-flex align-items-center">
                                <div class="stat-icon me-3">
                                    <i class="bi bi-folder"></i>
                                </div>
                                <div class="flex-grow-1">
                                    <p class="stat-label mb-1">Total Repositories</p>
                                    <h2 class="stat-value mb-0">{{len .Repositories}}</h2>
                                </div>
                            </div>
                        </div>
                    </div>
                </div>

                <div class="col-md-4">
                    <div class="stat-card warning fade-in">
                        <div class="card-body p-4">
                            <div class="d-flex align-items-center">
                                <div class="stat-icon me-3">
                                    <i class="bi bi-hdd"></i>
                                </div>
                                <div class="flex-grow-1">
                                    <p class="stat-label mb-1">Total Size</p>
                                    <h2 class="stat-value mb-0">{{formatBytes .TotalSize}}</h2>
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
                    <i class="bi bi-plus-circle me-2"></i>Create New Repository
                </button>
            </div>

            <!-- Repositories List -->
            <div class="repo-card fade-in">
                <div class="repo-card-header">
                    <h5 class="card-title mb-0">
                        <i class="bi bi-collection me-2"></i>Repositories
                    </h5>
                    <small class="opacity-75">List of all repositories and their tags</small>
                </div>
                <div class="card-body p-0">
                    {{if .Repositories}}
                        <div>
                            {{range .Repositories}}
                            {{$repoName := .Name}}
                            <div class="repo-item">
                                <div class="d-flex justify-content-between align-items-start">
                                    <div class="flex-grow-1">
                                        <div class="repo-name">
                                            <i class="bi bi-folder-fill"></i>
                                            {{.Name}}
                                        </div>
                                        <div class="d-flex flex-wrap">
                                            {{if .Tags}}
                                                {{range .Tags}}
                                                <span class="tag-badge">
                                                    <i class="bi bi-tag"></i>
                                                    <span>{{.Name}}</span>
                                                    <small>({{formatBytes .Size}})</small>
                                                    <button @click="deleteTag('{{$repoName}}', '{{.Name}}')" 
                                                            class="tag-close" 
                                                            title="Delete tag">
                                                        <i class="bi bi-x"></i>
                                                    </button>
                                                </span>
                                                {{end}}
                                            {{else}}
                                                <span class="badge bg-secondary">
                                                    <i class="bi bi-inbox me-1"></i>No images yet
                                                </span>
                                            {{end}}
                                        </div>
                                    </div>
                                    <div class="ms-3">
                                        <button @click="deleteRepository('{{.Name}}')" 
                                                class="btn btn-outline-danger btn-sm btn-delete">
                                            <i class="bi bi-trash me-1"></i>Delete
                                        </button>
                                    </div>
                                </div>
                            </div>
                            {{end}}
                        </div>
                    {{else}}
                        <div class="empty-state">
                            <i class="bi bi-folder-x"></i>
                            <h5>No repositories found</h5>
                            <p>Create your first repository to get started with Docker image storage.</p>
                        </div>
                    {{end}}
                </div>
            </div>
        </div>
    </div>

    <!-- Create Repository Modal -->
    <div class="modal fade" id="createModal" tabindex="-1" aria-labelledby="createModalLabel" aria-hidden="true">
        <div class="modal-dialog modal-dialog-centered">
            <div class="modal-content">
                <div class="modal-header">
                    <h5 class="modal-title" id="createModalLabel">
                        <i class="bi bi-plus-circle me-2"></i>Create New Repository
                    </h5>
                    <button type="button" class="btn-close" data-bs-dismiss="modal" aria-label="Close" @click="hideCreateModal()"></button>
                </div>
                <div class="modal-body p-4">
                    <div class="mb-3">
                        <label for="repo-name" class="form-label">Repository Name</label>
                        <input type="text" 
                               id="repo-name"
                               x-model="newRepoName" 
                               placeholder="Enter repository name (e.g., myapp)" 
                               class="form-control"
                               @keyup.enter="createRepository()"
                               autofocus>
                        <div x-show="createMessage" 
                             x-text="createMessage" 
                             :class="createSuccess ? 'text-success mt-2' : 'text-danger mt-2'" 
                             class="d-flex align-items-center">
                            <i x-show="createSuccess" class="bi bi-check-circle me-2"></i>
                            <i x-show="!createSuccess && createMessage" class="bi bi-exclamation-circle me-2"></i>
                        </div>
                    </div>
                </div>
                <div class="modal-footer">
                    <button type="button" class="btn btn-secondary" data-bs-dismiss="modal" @click="hideCreateModal()">
                        Cancel
                    </button>
                    <button type="button" 
                            class="btn btn-primary" 
                            @click="createRepository()"
                            :disabled="isCreating">
                        <span x-show="!isCreating">
                            <i class="bi bi-check-lg me-2"></i>Create Repository
                        </span>
                        <span x-show="isCreating">
                            <span class="loading me-2"></span>Creating...
                        </span>
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
                isCreating: false,
                isRefreshing: false,
                
                init() {
                    // Initialize Bootstrap modal
                    this.modal = new bootstrap.Modal(document.getElementById('createModal'));
                },
                
                showCreateModal() {
                    this.modal.show();
                    // Focus input after modal is shown
                    setTimeout(() => {
                        document.getElementById('repo-name').focus();
                    }, 300);
                },
                
                hideCreateModal() {
                    this.modal.hide();
                    this.newRepoName = '';
                    this.createMessage = '';
                    this.createSuccess = false;
                    this.isCreating = false;
                },
                
                async createRepository() {
                    if (!this.newRepoName.trim()) {
                        this.createMessage = 'Repository name is required';
                        this.createSuccess = false;
                        return;
                    }

                    this.isCreating = true;
                    this.createMessage = '';

                    // Get credentials if not already set
                    if (!this.credentials) {
                        const username = prompt('Enter username:');
                        const password = prompt('Enter password:');
                        if (!username || !password) {
                            this.createMessage = 'Authentication required';
                            this.createSuccess = false;
                            this.isCreating = false;
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
                            this.createMessage = result.message || 'Repository created successfully!';
                            this.createSuccess = true;
                            this.newRepoName = '';
                            setTimeout(() => {
                                this.createMessage = '';
                                this.hideCreateModal();
                                window.location.reload();
                            }, 1500);
                        } else {
                            this.createMessage = result.message || 'Failed to create repository';
                            this.createSuccess = false;
                        }
                    } catch (error) {
                        this.createMessage = 'Error creating repository: ' + error.message;
                        this.createSuccess = false;
                    } finally {
                        this.isCreating = false;
                    }
                },

                async deleteRepository(repoName) {
                    if (!confirm('Are you sure you want to delete repository "' + repoName + '" and all its tags?\n\nThis action cannot be undone.')) {
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
                        alert('Error deleting repository: ' + error.message);
                    }
                },

                async deleteTag(repoName, tagName) {
                    if (!confirm('Are you sure you want to delete tag "' + tagName + '" from repository "' + repoName + '"?\n\nThis action cannot be undone.')) {
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
                        alert('Error deleting tag: ' + error.message);
                    }
                },

                refreshData() {
                    this.isRefreshing = true;
                    setTimeout(() => {
                        window.location.reload();
                    }, 300);
                }
            }
        }
    </script>
</body>
</html>
`

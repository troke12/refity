import { useState, useEffect } from 'react';
import { useParams, useNavigate, Link } from 'react-router-dom';
import { groupsAPI, authAPI } from '../services/api';
import { formatBytes } from '../utils/formatBytes';
import './GroupPage.css';

function GroupPage() {
  const { groupName } = useParams();
  const navigate = useNavigate();
  const [data, setData] = useState(null);
  const [isLoading, setIsLoading] = useState(true);
  const [isRefreshing, setIsRefreshing] = useState(false);

  const loadData = async () => {
    try {
      const decodedGroup = decodeURIComponent(groupName);
      const groupData = await groupsAPI.getRepositories(decodedGroup);
      setData(groupData);
    } catch (error) {
      console.error('Failed to load group data:', error);
    } finally {
      setIsLoading(false);
      setIsRefreshing(false);
    }
  };

  useEffect(() => {
    loadData();
  }, [groupName]);

  const handleRefresh = () => {
    setIsRefreshing(true);
    loadData();
  };

  const handleLogout = async () => {
    if (window.confirm('Are you sure you want to logout?')) {
      await authAPI.logout();
      navigate('/login');
    }
  };

  if (isLoading) {
    return (
      <div className="d-flex justify-content-center align-items-center" style={{ minHeight: '100vh' }}>
        <div className="spinner-border text-primary" role="status">
          <span className="visually-hidden">Loading...</span>
        </div>
      </div>
    );
  }

  const decodedGroup = decodeURIComponent(groupName);

  return (
    <div className="container-main">
      <nav className="navbar navbar-expand-lg navbar-dark mb-4">
        <div className="container-fluid">
          <Link to="/" className="navbar-brand text-decoration-none">
            <i className="bi bi-box-seam me-2"></i>Refity Docker Registry
          </Link>
          <div className="navbar-nav ms-auto d-flex flex-row gap-2">
            <button
              onClick={handleRefresh}
              className="btn btn-outline-light"
              disabled={isRefreshing}
            >
              {isRefreshing ? (
                <span className="spinner-border spinner-border-sm me-2"></span>
              ) : (
                <i className="bi bi-arrow-clockwise me-1"></i>
              )}
              Refresh
            </button>
            <button onClick={handleLogout} className="btn btn-outline-light">
              <i className="bi bi-box-arrow-right me-1"></i>Logout
            </button>
          </div>
        </div>
      </nav>

      <div className="container-fluid px-0">
        <nav aria-label="breadcrumb" className="mb-4">
          <ol className="breadcrumb">
            <li className="breadcrumb-item">
              <Link to="/">Dashboard</Link>
            </li>
            <li className="breadcrumb-item active" aria-current="page">
              {decodedGroup}
            </li>
          </ol>
        </nav>

        <div className="card">
          <div className="card-header">
            <h5 className="mb-0">
              <i className="bi bi-folder me-2"></i>Group: {decodedGroup}
            </h5>
            <small className="text-muted">List of repositories in this group</small>
          </div>
          <div className="card-body">
            {data?.repositories && data.repositories.length > 0 ? (
              <div className="row g-3">
                {data.repositories.map((repo) => {
                  const totalSize = repo.tags.reduce((sum, tag) => sum + tag.size, 0);
                  return (
                    <div key={repo.name} className="col-md-4">
                      <Link
                        to={`/group/${encodeURIComponent(data.group)}/repository/${encodeURIComponent(repo.name)}`}
                        className="text-decoration-none"
                      >
                        <div className="card h-100 repository-card">
                          <div className="card-body">
                            <h6 className="card-title">
                              <i className="bi bi-box-seam me-2 text-primary"></i>
                              {repo.name}
                            </h6>
                            <p className="card-text text-muted mb-1">
                              <i className="bi bi-tag me-1"></i>
                              {repo.tags.length} {repo.tags.length === 1 ? 'tag' : 'tags'}
                            </p>
                            <p className="card-text text-muted mb-0">
                              <i className="bi bi-hdd me-1"></i>
                              {formatBytes(totalSize)}
                            </p>
                          </div>
                        </div>
                      </Link>
                    </div>
                  );
                })}
              </div>
            ) : (
              <p className="text-muted mb-0">No repositories found in this group.</p>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}

export default GroupPage;

